package pages

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/clashapi"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/platform"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func pageFixture(t *testing.T) *application.App {
	t.Helper()
	t.Setenv("KARING_HOME", t.TempDir())
	paths, err := platform.NewPaths()
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.New(paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	return app
}

func chars(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func acceptConfirm(t *testing.T, p Page) {
	t.Helper()
	_, cmd := p.Update(chars("y"))
	if cmd == nil {
		t.Fatal("missing confirmation result")
	}
	p.Update(cmd())
}

func TestFormErrorsStayVisibleWhileEditing(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.openSubForm("add-sub")
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if p.mode != profilesForm || p.form.Error == "" || p.form.CurrentKey() != "name" || !strings.Contains(p.View(), "订阅名称") {
		t.Fatal("subscription form hid its validation error")
	}
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.openForm("add-group")
	g.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if g.mode != groupsForm || g.form.Error == "" || g.form.CurrentKey() != "name" || !strings.Contains(g.View(), "代理组名称") {
		t.Fatal("group form hid its validation error")
	}
	d := NewDNS(app)
	d.SetSize(80, 21)
	d.reload()
	d.openForm("add-server")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if d.mode != dnsForm || d.form.Error == "" || d.form.CurrentKey() != "tag" || !strings.Contains(d.View(), "错误") {
		t.Fatal("DNS form hid its validation error")
	}
	d.openOptions()
	d.form.SetValueByKey("fakeip", "true")
	d.form.SetValueByKey("range", "bad-range")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if d.mode != dnsOptions || d.err == nil || !strings.Contains(d.View(), "网段非法") {
		t.Fatal("DNS options hid its validation error")
	}
}

func TestRoutingTargetsPreserveNamesAndDefaultGroupCanSave(t *testing.T) {
	app := pageFixture(t)
	if _, err := app.Proxy.CreateGroup("MiXeD组", "select", "", 0, nil); err != nil {
		t.Fatal(err)
	}
	r := NewRules(app)
	r.SetSize(80, 21)
	r.reload()
	for _, target := range []string{"Auto", "Manual", "MiXeD组", "direct", "bLoCk"} {
		r.openForm("add-rg")
		r.form.SetValueByKey("name", "测试 "+target)
		r.form.SetValueByKey("target", target)
		r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
		if r.err != nil {
			t.Fatalf("saving target %s: %v", target, r.err)
		}
		want := target
		if strings.EqualFold(target, "DIRECT") || strings.EqualFold(target, "BLOCK") {
			want = strings.ToUpper(target)
		}
		found := false
		for _, g := range r.groups {
			if g.Name == "测试 "+target {
				found = true
				if g.Target != want {
					t.Fatalf("target rewritten to %q", g.Target)
				}
			}
		}
		if !found {
			t.Fatal("routing group not saved")
		}
	}
	for _, g := range r.groups {
		if g.Name == "Telegram" {
			r.openFormEditGroup(g)
			r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
			if r.err != nil {
				t.Fatalf("default Telegram edit: %v", r.err)
			}
			return
		}
	}
	t.Fatal("fixture lacked default Telegram group")
}

func TestLongSubscriptionURLAndMixedBatchImport(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	url := "https://example.invalid/" + strings.Repeat("a", 661-len("https://example.invalid/"))
	p.openSubForm("add-sub")
	p.form.SetValueByKey("name", "long")
	p.form.SetValueByKey("url", url)
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if p.err != nil {
		t.Fatal(p.err)
	}
	subs, _ := app.DB.ListSubscriptions()
	if len(subs) != 1 || subs[0].URL != url {
		t.Fatal("URL did not survive form-to-database round trip")
	}
	var links []string
	for i := range 10 {
		links = append(links, fmt.Sprintf("trojan://fixture-password@node.example.invalid:443#node-%02d", i))
	}
	links = append(links, "not-a-link")
	p.openImportForm()
	p.form.SetValueByKey("links", strings.Join(links, "\n"))
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	nodes, err := app.DB.ListNodes(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 10 || len(p.results) != 11 || p.err == nil {
		t.Fatalf("partial import not accounted for: nodes=%d results=%d err=%v", len(nodes), len(p.results), p.err)
	}
	if p.results[10].State != "失败" {
		t.Fatal("invalid link was silently skipped")
	}
}

func TestBatchUpdateSkipsDisabledReportsEveryResultAndRetriesOnlyFailures(t *testing.T) {
	app := pageFixture(t)
	var mu sync.Mutex
	counts := map[string]int{}
	var recoverBad atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		if r.URL.Path == "/bad" && !recoverBad.Load() {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "trojan://fixture-password@node.example.invalid:443#fixture")
	}))
	defer server.Close()
	var disabledID int64
	for _, name := range []string{"good", "bad", "disabled"} {
		sub, err := app.Subs.Add(context.Background(), name, server.URL+"/"+name, "")
		if err != nil {
			t.Fatal(err)
		}
		if name == "disabled" {
			disabledID = sub.ID
			if err := app.Subs.SetEnabled(sub.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	p := NewProfiles(app)
	p.reload()
	_, cmd := p.Update(chars("U"))
	if cmd == nil {
		t.Fatal("batch did not start")
	}
	if _, duplicate := p.Update(chars("U")); duplicate != nil {
		t.Fatal("duplicate update was allowed")
	}
	p.Update(cmd())
	if p.busy || p.err == nil || !strings.Contains(p.status, "成功 1 · 失败 1 · 跳过 1") {
		t.Fatalf("wrong batch result: %s, %v", p.status, p.err)
	}
	p.Update(chars("r"))
	if p.err == nil {
		t.Fatal("refresh erased an unacknowledged failure")
	}
	p.SetSize(80, 21)
	p.Update(ActivateMsg{})
	view := p.View()
	for _, count := range []string{"成功 1", "失败 1", "跳过 1"} {
		if !strings.Contains(view, count) {
			t.Fatalf("partial update summary hidden after returning: %s", view)
		}
	}
	disabled, _ := app.DB.GetSubscription(disabledID)
	if !disabled.LastUpdated.IsZero() {
		t.Fatal("disabled subscription was changed")
	}
	recoverBad.Store(true)
	_, cmd = p.Update(chars("f"))
	if cmd == nil {
		t.Fatal("failed subscription cannot be retried")
	}
	p.Update(cmd())
	mu.Lock()
	defer mu.Unlock()
	if counts["/good"] != 1 || counts["/bad"] != 2 || counts["/disabled"] != 0 {
		t.Fatalf("wrong retry/download boundary: %v", counts)
	}
	if p.busy || p.err != nil || !strings.Contains(p.status, "成功 1 · 失败 0") {
		t.Fatalf("retry stayed busy/failed: %s %v", p.status, p.err)
	}
}

func TestStaleTaskCannotClearNewTask(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	old := p.task("old", func() tea.Msg { return actionDoneMsg{Action: "old"} })()
	if duplicate := p.task("duplicate", func() tea.Msg { t.Fatal("duplicate task ran"); return nil }); duplicate != nil {
		t.Fatal("duplicate task was scheduled")
	}
	p.Update(old)
	newCmd := p.task("new", func() tea.Msg { return actionDoneMsg{Action: "new"} })
	p.busy = true
	p.Update(old)
	if !p.busy || !p.taskActive {
		t.Fatal("stale result cleared a new operation")
	}
	p.Update(newCmd())
	if p.busy || p.taskActive {
		t.Fatal("matching result did not release busy state")
	}
}

func TestOldDashboardSpeedTestCannotClearNewTask(t *testing.T) {
	d := NewDashboard(pageFixture(t))
	d.testBusy, d.testID = true, 2
	d.Update(testDoneMsg{id: 1, err: fmt.Errorf("old failure")})
	if !d.testBusy || d.lastErr != nil {
		t.Fatal("old speed test completion changed the current task")
	}
	active, label := d.TaskStatus()
	if !active || !strings.Contains(label, "测速") {
		t.Fatal("background speed test is missing from global feedback")
	}
}

func TestDynamicGroupSelectionAndReturnPosition(t *testing.T) {
	app := pageFixture(t)
	sub, err := app.Subs.Add(context.Background(), "synthetic", "http://example.invalid", "")
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*config.Node
	for i := range 1000 {
		nodes = append(nodes, &config.Node{Name: fmt.Sprintf("node-%04d 中文👩‍💻é", i), Protocol: "trojan", Server: "example.invalid", Port: 443, Enabled: true, Metadata: map[string]any{"password": "fixture"}})
	}
	if err := app.DB.ReplaceSubscriptionNodes(sub.ID, nodes, time.Now()); err != nil {
		t.Fatal(err)
	}
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.reload()
	var id int64
	for _, group := range g.groups {
		if group.Name == "Manual" {
			id = group.ID
		}
	}
	g.list.SelectKey(strconv.FormatInt(id, 10))
	g.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if !g.detailActive || !strings.Contains(g.View(), "Manual") {
		t.Fatal("full group details are not reachable")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(g.members) != 1000 {
		t.Fatalf("dynamic group has %d choices", len(g.members))
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(ansi.Strip(g.View()), "> node-0999") {
		t.Fatal("selected last member is invisible")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	before, _ := app.DB.GetProxyGroup(id)
	if before.Selected != "" {
		t.Fatal("browsing changed the selected proxy")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	chosen, _ := g.curMember()
	g.Update(chars(" "))
	after, _ := app.DB.GetProxyGroup(id)
	if after.Selected != chosen.key || !strings.Contains(g.status, "应用后生效") {
		t.Fatal("persistent selection lacks correct pending-apply feedback")
	}
	if err := app.Proxy.SetGroupSelected(id, "all"); err == nil {
		t.Fatal("dynamic placeholder is still selectable")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	current, ok := g.selectedGroup()
	if !ok || current.ID != id {
		t.Fatal("return did not restore the original group")
	}
}

func TestDNSMoveEditDeleteAndReferenceErrorKeepIdentity(t *testing.T) {
	app := pageFixture(t)
	a, err := app.DNS.AddRule("domain", "a.example", "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DNS.AddRule("domain", "b.example", "local"); err != nil {
		t.Fatal(err)
	}
	d := NewDNS(app)
	d.SetSize(80, 21)
	d.reload()
	d.Update(chars("R"))
	d.list.SelectKey(fmt.Sprintf("rule:%d", a.ID))
	for _, key := range []string{"J", "J", "K", "K", "K", "J"} {
		d.Update(chars(key))
		rules, i, ok := d.selectedRule()
		if !ok || rules[i].ID != a.ID {
			t.Fatalf("%s changed the selected rule", key)
		}
	}
	d.Update(chars("e"))
	d.form.SetValueByKey("value", "a.changed")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	rules, i, ok := d.selectedRule()
	if !ok || rules[i].ID != a.ID || rules[i].Value != "a.changed" {
		t.Fatal("edit after move targeted another rule")
	}
	d.Update(chars("D"))
	_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyEnter})
	d.Update(cmd())
	rules, i, ok = d.selectedRule()
	if !ok || rules[i].ID != a.ID {
		t.Fatal("default delete cancellation changed selection")
	}
	d.Update(chars("d"))
	acceptConfirm(t, d)
	remaining, _ := app.DNS.ListRules()
	for _, r := range remaining {
		if r.ID == a.ID {
			t.Fatal("explicit delete did not remove the same rule")
		}
	}
	d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	servers, _ := app.DNS.ListServers()
	for _, server := range servers {
		if server.Tag == "remote" {
			d.list.SelectKey(fmt.Sprintf("server:%d", server.ID))
		}
	}
	d.Update(chars("d"))
	acceptConfirm(t, d)
	if d.err == nil || !strings.Contains(d.View(), "final") {
		t.Fatal("reference protection error was erased")
	}
	d.Update(ActivateMsg{})
	if d.err == nil || !strings.Contains(d.View(), "final") {
		t.Fatal("activation erased delete failure")
	}
}

func TestNodeSearchIsIncrementalAndEscapeRestoresFilterAndPosition(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := app.Proxy.SaveManual(&config.Node{Name: name, Protocol: "http", Server: "example.invalid", Port: 80}); err != nil {
			t.Fatal(err)
		}
	}
	p.reload()
	p.Update(chars("n"))
	p.list.Cursor = 1
	before := p.list.SelectedKey()
	p.Update(chars("/"))
	p.Update(chars("gamma"))
	if len(p.filtered) != 1 || !strings.Contains(p.View(), "gamma") {
		t.Fatal("search hid the results instead of filtering them")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if p.search != "" || len(p.filtered) != 3 || p.list.SelectedKey() != before {
		t.Fatal("search cancel did not restore original selection")
	}
}

func TestDNSOptionsReturnToTheSameSubviewAndObject(t *testing.T) {
	app := pageFixture(t)
	if _, err := app.DNS.AddRule("domain", "options.example", "local"); err != nil {
		t.Fatal(err)
	}
	d := NewDNS(app)
	d.SetSize(80, 21)
	d.reload()
	for _, mode := range []dnsMode{dnsServers, dnsRules} {
		d.mode = mode
		d.reload()
		d.Update(tea.KeyMsg{Type: tea.KeyEnd})
		before := d.list.SelectedKey()
		d.Update(chars("F"))
		d.Update(tea.WindowSizeMsg{Width: 120, Height: 27})
		d.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if d.mode != mode || d.list.SelectedKey() != before {
			t.Fatal("options cancellation changed the subview or selection")
		}
		d.Update(chars("F"))
		d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
		if d.err != nil || d.mode != mode || d.list.SelectedKey() != before {
			t.Fatalf("options save changed the subview or selection: %v", d.err)
		}
	}
}

func TestMemberPickerSearchEscapeRestoresFilterAndPosition(t *testing.T) {
	app := pageFixture(t)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := app.Proxy.SaveManual(&config.Node{Name: name, Protocol: "http", Server: "example.invalid", Port: 80}); err != nil {
			t.Fatal(err)
		}
	}
	g := NewGroups(app)
	g.SetSize(80, 21)
	g.reload()
	g.Update(tea.KeyMsg{Type: tea.KeyEnter})
	g.Update(chars("m"))
	g.Update(tea.KeyMsg{Type: tea.KeyEnd})
	before := g.pickList.SelectedKey()
	g.Update(chars("/"))
	g.Update(chars("beta"))
	if len(g.pickList.Items) != 1 || !strings.Contains(g.View(), "beta") {
		t.Fatal("member search did not retain the filtered list")
	}
	g.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if g.pickQuery != "" || g.pickList.SelectedKey() != before {
		t.Fatal("member search cancellation did not restore position")
	}
}

func TestLogsKeepAbsoluteAnchorAsNewLinesArrive(t *testing.T) {
	app := pageFixture(t)
	l := NewLogs(app)
	l.SetSize(80, 10)
	for i := range 100 {
		app.AppLog.AppendLine(fmt.Sprintf("fixture-line-%03d", i))
	}
	l.Update(tea.KeyMsg{Type: tea.KeyUp})
	before := strings.Join(strings.Split(l.View(), "\n")[1:8], "\n")
	for i := 100; i < 110; i++ {
		app.AppLog.AppendLine(fmt.Sprintf("fixture-line-%03d", i))
	}
	l.Update(tickMsg(time.Now()))
	after := strings.Join(strings.Split(l.View(), "\n")[1:8], "\n")
	if before != after {
		t.Fatal("new logs moved the reader's anchor")
	}
	l.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if !strings.Contains(l.View(), "fixture-line-109") {
		t.Fatal("End did not resume following")
	}
	if _, cmd := l.Update(ActivateMsg{}); cmd != nil {
		t.Fatal("activation duplicated the refresh chain")
	}
}

func TestDashboardRejectsOldInstanceAndResetsRateBaseline(t *testing.T) {
	app := pageFixture(t)
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
	d := NewDashboard(app)
	d.fetchID = 1
	instance := app.Core.Status().StartedAt
	t0 := time.Now()
	sample := func(n int64, at time.Time) {
		d.Update(snapMsg{id: 1, startedAt: instance, sampledAt: at, conns: clashapi.Connections{UploadTotal: n, DownloadTotal: n}})
	}
	sample(1000, t0)
	sample(3000, t0.Add(time.Second))
	if d.upSpeed != 2000 || d.downSpeed != 2000 {
		t.Fatal("normal rates are incorrect")
	}
	sample(100, t0.Add(2*time.Second))
	if d.upSpeed != 0 || d.downSpeed != 0 {
		t.Fatal("counter rollback produced a rate spike or negative rate")
	}
	d.Update(snapMsg{id: 1, startedAt: instance, err: fmt.Errorf("offline")})
	sample(5000, t0.Add(3*time.Second))
	if d.upSpeed != 0 || d.downSpeed != 0 {
		t.Fatal("reconnect did not rebuild the baseline")
	}
	if err := app.RestartCore(context.Background()); err != nil {
		t.Fatal(err)
	}
	sample(999999, t0.Add(4*time.Second))
	if d.upTotal == 999999 {
		t.Fatal("snapshot from old core was accepted")
	}
	if _, cmd := d.Update(ActivateMsg{}); cmd != nil {
		t.Fatal("activation duplicated the refresh chain")
	}
}
