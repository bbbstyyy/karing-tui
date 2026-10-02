package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/platform"
)

func TestTUIStabilityErrorCleanup(t *testing.T) {
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in\nversion) echo 'sing-box version 9.9.9';;\nrun) trap 'exit 0' TERM; while :; do sleep 1; done;;\nesac\n"
	if err := os.WriteFile(paths.CoreBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected terminal disconnect")
	var captured *application.App
	t.Cleanup(func() {
		if captured != nil {
			_ = captured.Core.Stop()
		}
	})
	err = runTUI(func(app *application.App) error {
		captured = app
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Core.Start(ctx, paths.Config); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("terminal error: %v", err)
	}
	if captured == nil || captured.Core.IsRunning() {
		t.Fatal("terminal failure left the core running")
	}
	if _, err := captured.DB.TableCount("settings"); err == nil {
		t.Fatal("terminal failure left the database open")
	}
	reopened, err := application.NewExclusive(paths)
	if err != nil {
		t.Fatalf("instance lock was not released: %v", err)
	}
	defer reopened.Close()
}
