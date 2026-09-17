package application

import (
	"archive/zip"
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bbbstyyy/karing-tui/internal/storage"
)

func startRestoreFixtureCore(t *testing.T, app *App) {
	t.Helper()
	// StartCore 会先生成配置；C15 起没有可用节点时生成会 fail-closed，
	// 所以「启动核心」这个动作本身就以「库里有可用节点」为前提。
	seedEnabledNode(t, app)
	script := `#!/bin/sh
case "$1" in
 version) echo 'sing-box version 1.14.0'; exit 0 ;;
 check) exit 0 ;;
 run) trap 'exit 0' TERM; while :; do sleep 1; done ;;
esac
exit 1
`
	if err := os.WriteFile(app.Paths.CoreBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.StartCore(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func restoreZip(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	w, err := z.Create("karing.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRestorePreflightRejectsBadFilesWithoutStoppingCore(t *testing.T) {
	app, paths := setupApp(t)
	if _, err := app.Subs.Add(context.Background(), "retained", "http://example.invalid", ""); err != nil {
		t.Fatal(err)
	}
	startRestoreFixtureCore(t, app)
	before := app.Core.Status()
	badZip := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(badZip, []byte("not zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrongDB := filepath.Join(t.TempDir(), "wrong.db")
	db, err := sql.Open("sqlite", wrongDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=7; CREATE TABLE unrelated (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	wrongBytes, err := os.ReadFile(wrongDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, archive := range []string{filepath.Join(t.TempDir(), "missing.zip"), badZip, restoreZip(t, []byte("SQLite format 3\x00corrupt")), restoreZip(t, wrongBytes)} {
		if prepared, err := PrepareRestore(paths, archive); err == nil {
			prepared.Close()
			t.Fatalf("accepted invalid backup %s", archive)
		}
		after := app.Core.Status()
		if !app.Core.IsRunning() || !after.StartedAt.Equal(before.StartedAt) {
			t.Fatal("preflight failure interrupted the running core")
		}
		subs, err := app.DB.ListSubscriptions()
		if err != nil || len(subs) != 1 || subs[0].Name != "retained" {
			t.Fatal("preflight failure changed the active database")
		}
	}
}

func TestPreparedRestoreUsesValidatedCopyAndBusyWorkPreservesService(t *testing.T) {
	app, paths := setupApp(t)
	if _, err := app.Subs.Add(context.Background(), "backed up", "http://example.invalid/a", ""); err != nil {
		t.Fatal(err)
	}
	archive, err := app.Backup(filepath.Join(t.TempDir(), "valid.zip"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRestore(paths, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Subscriptions != 1 {
		t.Fatal("preflight summary did not match the archive")
	}
	if _, err := app.Subs.Add(context.Background(), "new data", "http://example.invalid/b", ""); err != nil {
		t.Fatal(err)
	}
	startRestoreFixtureCore(t, app)
	release, err := app.BeginOperation()
	if err != nil {
		t.Fatal(err)
	}
	err = app.RestorePrepared(context.Background(), prepared)
	release()
	if err == nil || !app.Core.IsRunning() {
		t.Fatal("restore interrupted an in-flight operation")
	}
	// Changing the source after confirmation must not change what gets restored.
	if err := os.WriteFile(archive, []byte("changed after preflight"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := app.RestorePrepared(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	subs, err := db.ListSubscriptions()
	if err != nil || len(subs) != 1 || subs[0].Name != "backed up" {
		t.Fatal("restore did not use the prevalidated snapshot")
	}
	if _, err := os.Stat(paths.DB + ".pre-restore"); err != nil {
		t.Fatal("pre-restore database backup missing")
	}
}

func TestReplacementFailureRollsBackDatabaseAndRestartsOldCore(t *testing.T) {
	app, paths := setupApp(t)
	if _, err := app.Subs.Add(context.Background(), "original", "http://example.invalid", ""); err != nil {
		t.Fatal(err)
	}
	archive, err := app.Backup(filepath.Join(t.TempDir(), "valid.zip"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRestore(paths, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	startRestoreFixtureCore(t, app)
	// Simulate a disk fault after preflight, forcing the post-replacement check.
	if err := os.WriteFile(prepared.database, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = app.RestorePrepared(context.Background(), prepared)
	if err == nil || !strings.Contains(err.Error(), "回滚") || !strings.Contains(err.Error(), "已恢复运行") {
		t.Fatalf("missing rollback/runtime outcome: %v", err)
	}
	if !app.Core.IsRunning() {
		t.Fatal("old core was not restarted")
	}
	subs, readErr := app.DB.ListSubscriptions()
	if readErr != nil || len(subs) != 1 || subs[0].Name != "original" {
		t.Fatal("original data was not restored")
	}
}

func TestRestoreWaitsForReadersAndBacksUpCommittedWALData(t *testing.T) {
	app, paths := setupApp(t)
	if _, err := app.Subs.Add(context.Background(), "in archive", "http://example.invalid/a", ""); err != nil {
		t.Fatal(err)
	}
	archive, err := app.Backup(filepath.Join(t.TempDir(), "valid.zip"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareRestore(paths, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	startRestoreFixtureCore(t, app)
	before := app.Core.Status().StartedAt
	reader, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: paths.DB}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow("SELECT count(*) FROM subscriptions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	// This commit cannot be checkpointed past the reader's older snapshot.
	if _, err := app.Subs.Add(context.Background(), "committed in WAL", "http://example.invalid/b", ""); err != nil {
		t.Fatal(err)
	}
	if err := app.RestorePrepared(context.Background(), prepared); err == nil {
		t.Fatal("restore proceeded while a reader held the old WAL snapshot")
	}
	if !app.Core.IsRunning() || !before.Equal(app.Core.Status().StartedAt) {
		t.Fatal("busy database check interrupted the running core")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.RestorePrepared(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	backup, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: paths.DB + ".pre-restore"}).String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.QueryRow("SELECT count(*) FROM subscriptions").Scan(&count); err != nil || count != 2 {
		t.Fatalf("pre-restore backup lost committed WAL data: count=%d err=%v", count, err)
	}
}

func TestConfigStageTracksSavedGeneratedAndAppliedVersions(t *testing.T) {
	app, _ := setupApp(t)
	startRestoreFixtureCore(t, app)
	if app.ConfigStage() != "运行中已生效" {
		t.Fatal(app.ConfigStage())
	}
	oldClient := app.ClashClient()
	oldCoreSettings := app.CoreSettings()
	set := app.GetSettings()
	set.ClashAPIPort = 0
	app.SetSettings(set)
	app.MarkConfigDirty()
	if !strings.Contains(app.ConfigStage(), "待生成") {
		t.Fatal(app.ConfigStage())
	}
	if app.ClashClient() == nil || oldClient == nil {
		t.Fatal("saved API setting replaced the still-running API endpoint")
	}
	if app.CoreSettings().ClashAPIPort != oldCoreSettings.ClashAPIPort {
		t.Fatal("runtime settings changed before applying the saved settings")
	}
	if err := app.GenerateConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.ConfigStage() != "已生成 · 待校验" {
		t.Fatal(app.ConfigStage())
	}
	if err := app.CheckConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.ConfigStage() != "校验通过 · 待应用" {
		t.Fatal(app.ConfigStage())
	}
	if err := app.RestartCore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.ConfigStage() != "运行中已生效" || app.ClashClient() != nil {
		t.Fatal("applied state/API settings not updated")
	}
}
