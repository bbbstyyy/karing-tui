package pages

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bbbstyyy/karing-tui/internal/config"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestSaveAndUpdateReportsProgressWithoutBlockingBrowse(t *testing.T) {
	app := pageFixture(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "trojan://fixture-password@node.example.invalid:443#test")
	}))
	defer server.Close()
	defer close(release)
	p := NewProfiles(app)
	p.SetSize(120, 27)
	p.openSubForm("add-sub")
	p.form.SetValueByKey("name", "保存并更新")
	p.form.SetValueByKey("url", server.URL)
	_, cmd := p.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if cmd == nil || p.mode != profilesSubs || !p.busy {
		t.Fatal("save/update did not leave the form and start a task")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("download not started")
	}
	if _, label := p.TaskStatus(); !strings.Contains(label, "0/1") {
		t.Fatalf("missing live progress: %s", label)
	}
	if _, duplicate := p.Update(chars("U")); duplicate != nil {
		t.Fatal("duplicate download accepted")
	}
	p.Update(chars("]"))
	if p.mode != profilesNodes {
		t.Fatal("browsing blocked by download")
	}
	release <- struct{}{}
	select {
	case msg := <-result:
		p.Update(msg)
	case <-time.After(3 * time.Second):
		t.Fatal("download did not finish")
	}
	if requests.Load() != 1 || p.busy || len(p.nodes) != 1 || len(p.taskResults) != 1 {
		t.Fatal("download result was lost after navigation")
	}
}

func TestLogicalConditionRowsRoundTripAndCancel(t *testing.T) {
	app := pageFixture(t)
	group, err := app.Rout.CreateGroup("条件行", "Auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRules(app)
	r.SetSize(80, 21)
	r.cur = group
	r.openForm("add-rule")
	r.form.SetValueByKey("type", "logical")
	configureRuleValue(app, &r.form)
	r.form.FocusKey("value")
	r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if r.logical == nil {
		t.Fatal("logical field did not open row editor")
	}
	for _, size := range [][2]int{{60, 15}, {80, 21}, {120, 27}} {
		view := r.logical.view(size[0], size[1])
		if len(strings.Split(view, "\n")) > size[1] || !strings.Contains(view, "Ctrl+S") {
			t.Fatal("logical editor hides save controls at small sizes")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("logical editor overflows width")
			}
		}
	}
	r.Update(chars("a"))
	r.logical.form.SetValueByKey("type", "domain_regex")
	r.logical.form.SetValueByKey("value", `^example[0-9]{2,4}\.com$`)
	r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	r.Update(chars("a"))
	r.logical.form.SetValueByKey("value", "private.example")
	r.logical.form.SetValueByKey("invert", "true")
	r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	r.Update(chars("]"))
	r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if r.logical != nil || !strings.Contains(r.form.ValueByKey("value"), "||") {
		t.Fatal("conditions did not return to parent form")
	}
	r.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if r.err != nil || r.mode != rulesGroupRl {
		t.Fatalf("save failed: %v", r.err)
	}
	saved, err := app.DB.GetRoutingGroup(group.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule := saved.Rules[0]
	if rule.Mode != "or" || len(rule.Conditions) != 2 || !rule.Conditions[1].Invert || !strings.Contains(rule.Conditions[0].Value, "{2,4}") {
		t.Fatalf("conditions changed: %+v", rule)
	}
	r.openFormEditRule(0)
	r.form.FocusKey("value")
	r.Update(tea.KeyMsg{Type: tea.KeyEnter})
	before := r.form.ValueByKey("value")
	r.Update(chars(" "))
	r.Update(tea.KeyMsg{Type: tea.KeyEsc})
	r.Update(chars("y"))
	if r.logical != nil || r.form.ValueByKey("value") != before {
		t.Fatal("cancel changed the parent rule")
	}
}

func TestDNSSelectorsConditionalFieldsAndTabs(t *testing.T) {
	app := pageFixture(t)
	d := NewDNS(app)
	d.SetSize(120, 27)
	d.reload()
	selected := d.list.SelectedKey()
	d.Update(chars("]"))
	d.Update(chars("]"))
	if d.mode != dnsGlobal {
		t.Fatal("global options tab missing")
	}
	d.Update(chars("]"))
	if d.mode != dnsServers || d.list.SelectedKey() != selected {
		t.Fatal("tab round trip lost server")
	}
	d.openForm("add-server")
	if !d.form.Field("detour").Choice || len(d.form.Field("detour").Options) < 3 {
		t.Fatal("proxy references are not selectable")
	}
	d.mode = dnsServers
	d.openOptions()
	if strings.Contains(d.View(), "FakeIP 网段:") {
		t.Fatal("disabled FakeIP range still visible")
	}
	d.form.SetValueByKey("fakeip", "true")
	d.form.SetValueByKey("range", "invalid")
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	d.View()
	if d.form.CurrentKey() != "range" || !strings.Contains(d.View(), "网段非法") {
		t.Fatal("FakeIP error did not focus its field")
	}
	d.form.SetValueByKey("range", "198.18.0.0/15")
	d.optionsBack = dnsGlobal
	d.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if d.mode != dnsGlobal || !strings.Contains(d.View(), "已保存") {
		t.Fatal("global DNS tab hides save feedback")
	}
}

func TestInvalidSubscriptionFilterDoesNotCreatePartialSubscription(t *testing.T) {
	app := pageFixture(t)
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.openSubForm("add-sub")
	p.form.SetValueByKey("name", "完整保留")
	p.form.SetValueByKey("url", "https://example.invalid/sub")
	p.form.SetValueByKey("filter", "[")
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if p.form.CurrentKey() != "filter" || !p.form.ShowAdvanced || p.form.ValueByKey("name") != "完整保留" {
		t.Fatal("filter error did not reveal the field while preserving input")
	}
	subs, err := app.DB.ListSubscriptions()
	if err != nil || len(subs) != 0 {
		t.Fatalf("invalid filter left a partial subscription: %v %v", subs, err)
	}
}

func TestManualProtocolFieldsPreserveHiddenMetadata(t *testing.T) {
	app := pageFixture(t)
	node := &config.Node{Name: "保留高级字段", Protocol: "vless", Server: "node.example.invalid", Port: 443, TLS: true, Metadata: map[string]any{"uuid": "fixture-uuid", "reality_public_key": "fixture-public-key", "sni": "example.invalid", "alpn": []any{"h2"}}}
	if err := app.Proxy.SaveManual(node); err != nil {
		t.Fatal(err)
	}
	p := NewProfiles(app)
	p.SetSize(80, 21)
	p.openNodeForm(node)
	p.form.SetValueByKey("name", "改名")
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	if p.err != nil {
		t.Fatal(p.err)
	}
	saved, err := app.DB.GetNode(node.ID)
	if err != nil || saved.Name != "改名" || saved.Metadata["reality_public_key"] != "fixture-public-key" {
		t.Fatalf("advanced metadata lost: %+v, %v", saved, err)
	}
	p.openNodeForm(nil)
	p.form.SetValueByKey("protocol", "tuic")
	p.form.SetValueByKey("server", "tuic.example.invalid")
	p.form.SetValueByKey("port", "443")
	p.form.SetValueByKey("cred", "fixture-password")
	p.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	p.View()
	if p.form.CurrentKey() != "uuid" {
		t.Fatal("TUIC UUID validation focused the wrong credential")
	}
}

func TestPreviewFocusAndTablesAcrossSizes(t *testing.T) {
	app := pageFixture(t)
	p := NewRules(app)
	p.reload()
	for _, size := range [][2]int{{120, 27}, {160, 42}, {80, 21}, {60, 15}, {120, 27}} {
		p.SetSize(size[0], size[1])
		view := p.View()
		if len(strings.Split(view, "\n")) > size[1] {
			t.Fatal("table overflows height")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > size[0] {
				t.Fatal("table overflows width")
			}
		}
	}
	p.View()
	before := p.list.SelectedKey()
	p.Update(tea.KeyMsg{Type: tea.KeyTab})
	p.Update(tea.KeyMsg{Type: tea.KeyDown})
	if !p.previewFocus || p.list.SelectedKey() != before {
		t.Fatal("preview scroll moved the list selection")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !p.detailActive {
		t.Fatal("preview Enter is unavailable")
	}
	p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	p.Update(tea.KeyMsg{Type: tea.KeyTab})
	if p.list.SelectedKey() != before || p.previewFocus {
		t.Fatal("return did not restore list focus")
	}
}

func TestPartialRulesetDownloadReportsAndRetriesOnlyFailure(t *testing.T) {
	app := pageFixture(t)
	var good, bad atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			bad.Add(1)
			http.Error(w, "fixture", http.StatusServiceUnavailable)
			return
		}
		good.Add(1)
		fmt.Fprint(w, `{"version":3,"rules":[{"domain":["example.invalid"]}]}`)
	}))
	defer server.Close()
	a, err := app.Rules.AddRuleSet("成功规则集", "fixture-good", server.URL+"/good", "json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := app.Rules.AddRuleSet("失败规则集", "fixture-bad", server.URL+"/bad", "json")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRules(app)
	r.reload()
	cmd := r.downloadRuleSets([]int64{a.ID, b.ID}, "")
	r.Update(cmd())
	if len(r.taskResults) != 2 || !r.taskFailed || !strings.Contains(resultsText(r.taskResults), "失败规则集") {
		t.Fatal("missing per-object download failure")
	}
	_, cmd = r.Update(chars("f"))
	if cmd == nil {
		t.Fatal("retry missing")
	}
	r.Update(cmd())
	if good.Load() != 1 || bad.Load() != 2 {
		t.Fatal("retry repeated a successful download")
	}
}

func TestSettingsEditSingleFieldKeepsOtherSettings(t *testing.T) {
	app := pageFixture(t)
	s := NewSettings(app)
	s.SetSize(80, 21)
	s.reloadSettings()
	s.editSetting("resolve_ip_rules")
	s.form.SetValueByKey("resolve_ip_rules", "true")
	s.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	set := app.GetSettings()
	if s.err != nil || !set.ResolveIPRules || !set.PrivateDirect || set.MixedPort != config.DefaultSettings().MixedPort {
		t.Fatalf("single-field edit changed other settings: %+v, %v", set, s.err)
	}
	if !strings.Contains(s.settingsPreview(), "mixed") {
		t.Fatal("settings focus did not return")
	}
}
