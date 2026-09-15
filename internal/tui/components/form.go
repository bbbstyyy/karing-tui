package components

import (
	"errors"
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"strings"
	"unicode/utf8"

	"github.com/bbbstyyy/karing-tui/internal/tui/styles"
	"github.com/bbbstyyy/karing-tui/internal/validation"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type Option struct{ Value, Label string }

type FormField struct {
	Label, Key, Help            string
	Section                     string
	Advanced                    bool
	When                        func(*Form) bool
	Validate                    func(string) error
	Action                      string
	Choice, Multi               bool
	Limit                       int
	Secret, Revealed, Multiline bool
	Options                     []Option
	input                       textinput.Model
	area                        textarea.Model
}

func (f *FormField) Value() string {
	if f.Multiline {
		return f.area.Value()
	}
	return f.input.Value()
}

func (f *FormField) SetValue(v string) {
	if f.Multiline {
		f.area.SetValue(v)
	} else {
		f.input.SetValue(v)
	}
}

func (f *FormField) blur() { f.input.Blur(); f.area.Blur() }
func (f *FormField) focus() tea.Cmd {
	if f.Multiline {
		return f.area.Focus()
	}
	return f.input.Focus()
}

// Form retains all input, validates limits on save, and never submits from a
// text field's Enter key. All fields and the two explicit buttons are focusable.
type Form struct {
	Title                              string
	Fields                             []FormField
	Editing                            bool
	Error                              string
	Width, Height                      int
	focus, offset                      int
	baseline                           []string
	discard                            Confirm
	selecting                          bool
	options                            SimpleList
	query                              textinput.Model
	showError                          bool
	errorScroll                        TextView
	ShowAdvanced                       bool
	SaveLabel, ExtraAction, ExtraLabel string
	selection                          map[string]bool
	selectionOrder                     []string
	externalError                      string
}

func NewForm(title string, labels, keys, hints []string) Form {
	f := Form{Title: title}
	for i, label := range labels {
		input := textinput.New()
		input.Prompt = ""   // The field label already carries the focus marker.
		input.CharLimit = 0 // Input is never silently discarded.
		hint := ""
		if i < len(hints) {
			hint = hints[i]
		}
		input.Placeholder = hint
		field := FormField{Label: label, Key: keys[i], Help: hint, input: input, Limit: 4096}
		switch keys[i] {
		case "name", "tag":
			field.Limit = 1024
		case "url", "address", "download_proxy":
			field.Limit = 65536
			if keys[i] != "address" {
				field.Secret = true
				field.input.EchoMode, field.input.EchoCharacter = textinput.EchoPassword, '*'
			}
		case "value", "filter":
			field.Limit = 1 << 20
		case "links":
			field.Limit, field.Multiline = 10<<20, true
			field.area = textarea.New()
			field.area.CharLimit = 0
			field.area.ShowLineNumbers = true
			field.area.Placeholder = "每行一条分享链接，可一次粘贴多条"
		case "cred", "uuid", "clash_api_secret":
			field.Secret = true
			field.input.EchoMode, field.input.EchoCharacter = textinput.EchoPassword, '*'
		}
		f.Fields = append(f.Fields, field)
	}
	for _, key := range []string{"tls", "autotest", "autoclean", "allow_lan", "private_direct", "resolve_ip_rules", "fakeip", "invert"} {
		f.SetOptions(key, "false", "true")
	}
	f.SetChoices("download_strategy", []Option{{"prefer_proxy", "优先代理 · 失败后直连"}, {"prefer_direct", "优先直连 · 失败后代理"}, {"only_proxy", "仅代理 · 失败即报错"}, {"only_direct", "仅直连 · 不使用下载代理"}})
	f.SetChoices("sort", []Option{{"name", "名称"}, {"latency", "延迟 · 成功在前"}})
	f.SetChoices("log_level", []Option{{"debug", "debug · 调试"}, {"info", "info · 常规信息"}, {"warn", "warn · 警告"}, {"error", "error · 错误"}})
	f.SetChoices("strategy", []Option{{"prefer_ipv4", "优先 IPv4"}, {"prefer_ipv6", "优先 IPv6"}, {"ipv4_only", "仅 IPv4"}, {"ipv6_only", "仅 IPv6"}})
	f.SetOptions("format", "srs", "json")
	f.query = textinput.New()
	f.query.CharLimit = 0
	if len(f.Fields) > 0 {
		f.Fields[0].focus()
	}
	return f
}

func (f *Form) SetOptions(key string, values ...string) {
	opts := make([]Option, 0, len(values))
	for _, v := range values {
		opts = append(opts, Option{Value: v, Label: v})
	}
	f.SetChoices(key, opts)
}

func (f *Form) SetChoices(key string, opts []Option) {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			f.Fields[i].Choice = true
			f.Fields[i].Options = opts
			if f.Fields[i].Value() == "" && len(opts) > 0 {
				f.Fields[i].SetValue(opts[0].Value)
			}
		}
	}
}

// Field configures a field without depending on its position in the form.
func (f *Form) Field(key string) *FormField {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return &f.Fields[i]
		}
	}
	return nil
}

func (f *Form) relevant(i int) bool {
	return f.Fields[i].When == nil || f.Fields[i].When(f)
}

func (f *Form) visible(i int) bool {
	return f.relevant(i) && (!f.Fields[i].Advanced || f.ShowAdvanced)
}

func (f *Form) hasAdvanced() bool {
	for i := range f.Fields {
		if f.Fields[i].Advanced && f.relevant(i) {
			return true
		}
	}
	return false
}

func (f *Form) focusCount() int {
	n := len(f.Fields) + 2
	if f.ExtraAction != "" {
		n++
	}
	return n
}

func (f *Form) CurrentKey() string {
	if f.focus < len(f.Fields) {
		return f.Fields[f.focus].Key
	}
	return ""
}

func (f *Form) ValueByKey(key string) string {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			return f.Fields[i].Value()
		}
	}
	return ""
}

func (f *Form) SetValueByKey(key, value string) {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			f.Fields[i].SetValue(value)
		}
	}
}

func (f *Form) Reset() {
	f.ShowAdvanced = false
	f.showError = false
	for i := range f.Fields {
		value := ""
		if len(f.Fields[i].Options) > 0 {
			value = f.Fields[i].Options[0].Value
		}
		f.Fields[i].SetValue(value)
		f.Fields[i].blur()
		f.Fields[i].Revealed = false
		if f.Fields[i].Secret {
			f.Fields[i].input.EchoMode = textinput.EchoPassword
		}
	}
	f.focus, f.offset = 0, 0
	f.Error, f.baseline, f.selecting = "", nil, false
	f.externalError = ""
	f.discard = Confirm{}
	if len(f.Fields) > 0 {
		f.Fields[0].focus()
	}
}

// Begin records the original values after an edit form has been populated.
func (f *Form) Begin() {
	f.baseline = make([]string, len(f.Fields))
	for i := range f.Fields {
		f.baseline[i] = f.Fields[i].Value()
	}
}

func (f *Form) Dirty() bool {
	if f.baseline == nil {
		return false
	}
	for i := range f.Fields {
		if f.baseline[i] != f.Fields[i].Value() {
			return true
		}
	}
	return false
}

func (f *Form) SetError(err error) {
	if err == nil {
		f.Error = ""
		return
	}
	if f.Error == err.Error() || (f.externalError == err.Error() && f.Error != "") {
		return
	}
	f.externalError = err.Error()
	f.Error = err.Error()
	var fieldErr *validation.Error
	if errors.As(err, &fieldErr) {
		f.FocusKey(fieldErr.Field)
		return
	}
	for i, field := range f.Fields {
		if strings.Contains(strings.ToLower(f.Error), strings.ToLower(field.Key)) || strings.Contains(f.Error, field.Label) {
			if field.Advanced {
				f.ShowAdvanced = true
			}
			f.setFocus(i)
			break
		}
	}
}

func (f *Form) FocusKey(key string) {
	for i := range f.Fields {
		if f.Fields[i].Key == key {
			if f.Fields[i].Advanced {
				f.ShowAdvanced = true
			}
			f.setFocus(i)
			return
		}
	}
}

func (f *Form) setFocus(i int) tea.Cmd {
	for j := range f.Fields {
		f.Fields[j].blur()
	}
	direction := 1
	if i < f.focus {
		direction = -1
	}
	n := f.focusCount()
	f.focus = (i + n) % n
	for f.focus < len(f.Fields) && !f.visible(f.focus) {
		f.focus = (f.focus + direction + n) % n
	}
	if f.focus < len(f.Fields) {
		return f.Fields[f.focus].focus()
	}
	return nil
}

func (f *Form) validate() bool {
	f.Error = ""
	for i := range f.Fields {
		field := &f.Fields[i]
		if !f.relevant(i) {
			continue
		}
		if n := utf8.RuneCountInString(field.Value()); field.Limit > 0 && n > field.Limit {
			f.Error = fmt.Sprintf("%s 超过 %d 字符上限（当前 %d）；输入已保留，请缩短后保存", field.Label, field.Limit, n)
			f.FocusKey(field.Key)
			return false
		}
		if field.Validate != nil {
			if err := field.Validate(field.Value()); err != nil {
				f.Error = err.Error()
				f.FocusKey(field.Key)
				return false
			}
		}
		if field.Choice && field.Action == "" {
			values := []string{field.Value()}
			if field.Multi {
				values = strings.Split(field.Value(), ",")
			}
			for _, value := range values {
				found := false
				for _, opt := range field.Options {
					if strings.TrimSpace(value) == opt.Value {
						found = true
						break
					}
				}
				if !found && value != "" {
					f.Error = fmt.Sprintf("%s 的选项已不可用，请重新选择", field.Label)
					f.FocusKey(field.Key)
					return false
				}
			}
		}
	}
	return true
}

// Handle returns "save" or "cancel" only for the corresponding explicit action.
func (f *Form) Handle(msg tea.Msg) (string, tea.Cmd) {
	if f.baseline == nil {
		f.Begin()
	}
	if f.focus < len(f.Fields) && !f.visible(f.focus) {
		f.setFocus(f.focus + 1)
	}
	key, isKey := msg.(tea.KeyMsg)
	if f.showError {
		if isKey {
			switch key.String() {
			case keys.Cancel, "q", keys.Error:
				f.showError = false
			default:
				f.errorScroll.Move(key.String(), max(1, f.Height-2))
			}
		}
		return "", nil
	}
	if f.discard.Active {
		if isKey {
			if done, accept := f.discard.Choose(key.String()); done && accept {
				return "cancel", nil
			}
		}
		return "", nil
	}
	if f.selecting {
		return "", f.handleOptions(msg)
	}
	if isKey {
		switch key.String() {
		case keys.Advanced:
			f.ShowAdvanced = !f.ShowAdvanced
			if f.focus < len(f.Fields) && !f.visible(f.focus) {
				f.setFocus(0)
			}
			return "", nil
		case keys.SaveUpdate:
			if f.ExtraAction != "" && f.validate() {
				return f.ExtraAction, nil
			}
			return "", nil
		case keys.Error:
			if f.Error != "" {
				f.showError = true
				f.errorScroll.Offset = 0
			}
			return "", nil
		case keys.Save:
			if f.validate() {
				return "save", nil
			}
			return "", nil
		case keys.Cancel:
			return f.cancel()
		case keys.Focus:
			return "", f.setFocus(f.focus + 1)
		case keys.FocusBack:
			return "", f.setFocus(f.focus - 1)
		case keys.Enter:
			if f.ExtraAction != "" && f.focus == len(f.Fields)+2 {
				if f.validate() {
					return f.ExtraAction, nil
				}
				return "", nil
			}
			if f.focus == len(f.Fields) {
				if f.validate() {
					return "save", nil
				}
				return "", nil
			}
			if f.focus == len(f.Fields)+1 {
				return f.cancel()
			}
			field := &f.Fields[f.focus]
			if field.Action != "" {
				return field.Action, nil
			}
			if field.Choice {
				return "", f.openOptions()
			}
			if !field.Multiline {
				return "", f.setFocus(f.focus + 1)
			}
		case "up", "down":
			if f.focus >= len(f.Fields) || !f.Fields[f.focus].Multiline {
				delta := 1
				if key.String() == "up" {
					delta = -1
				}
				return "", f.setFocus(f.focus + delta)
			}
		case keys.Reveal:
			if f.focus < len(f.Fields) && f.Fields[f.focus].Secret {
				field := &f.Fields[f.focus]
				field.Revealed = !field.Revealed
				field.input.EchoMode = textinput.EchoPassword
				if field.Revealed {
					field.input.EchoMode = textinput.EchoNormal
				}
				return "", nil
			}
		case " ":
			if f.focus < len(f.Fields) && isBool(f.Fields[f.focus]) {
				field := &f.Fields[f.focus]
				next := "true"
				if field.Value() == "true" {
					next = "false"
				}
				field.SetValue(next)
				return "", nil
			}
		}
	}
	if f.focus >= len(f.Fields) {
		return "", nil
	}
	field := &f.Fields[f.focus]
	var cmd tea.Cmd
	if field.Choice || field.Action != "" {
		return "", nil
	}
	if field.Multiline {
		field.area, cmd = field.area.Update(msg)
	} else {
		field.input, cmd = field.input.Update(msg)
	}
	return "", cmd
}

func (f *Form) Update(msg tea.Msg) tea.Cmd { _, cmd := f.Handle(msg); return cmd }

func (f *Form) cancel() (string, tea.Cmd) {
	if !f.Dirty() {
		return "cancel", nil
	}
	f.discard = NewConfirm("discard", "放弃尚未保存的修改？取消可继续编辑。")
	return "", nil
}

func isBool(f FormField) bool {
	return len(f.Options) == 2 && f.Options[0].Value == "false" && f.Options[1].Value == "true"
}

func (f *Form) openOptions() tea.Cmd {
	f.selecting = true
	f.query.SetValue("")
	f.options = SimpleList{}
	f.selection, f.selectionOrder = map[string]bool{}, nil
	if f.Fields[f.focus].Multi {
		for _, value := range strings.Split(f.Fields[f.focus].Value(), ",") {
			value = strings.TrimSpace(value)
			if value != "" && !f.selection[value] {
				f.selection[value] = true
				f.selectionOrder = append(f.selectionOrder, value)
			}
		}
	}
	f.filterOptions()
	f.options.SelectKey(f.Fields[f.focus].Value())
	return f.query.Focus()
}

func (f *Form) filterOptions() {
	var items, keys []string
	for _, opt := range f.Fields[f.focus].Options {
		if strings.Contains(strings.ToLower(opt.Label+" "+opt.Value), strings.ToLower(f.query.Value())) {
			label := opt.Label
			if f.Fields[f.focus].Multi {
				mark := "[ ] "
				if f.selection[opt.Value] {
					mark = "[x] "
				}
				label = mark + label
			}
			items = append(items, label)
			keys = append(keys, opt.Value)
		}
	}
	f.options.SetItems(items, keys)
}

func (f *Form) handleOptions(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case keys.Cancel:
			f.selecting = false
			f.query.Blur()
			return nil
		case keys.Enter:
			if f.Fields[f.focus].Multi {
				var values []string
				for _, v := range f.selectionOrder {
					if f.selection[v] {
						values = append(values, v)
					}
				}
				f.Fields[f.focus].SetValue(strings.Join(values, ","))
				f.selecting = false
				f.query.Blur()
				return nil
			}
			if _, ok := f.options.Selected(); ok {
				f.Fields[f.focus].SetValue(f.options.SelectedKey())
				f.selecting = false
				f.query.Blur()
			}
			return nil
		case " ":
			if f.Fields[f.focus].Multi {
				if _, ok := f.options.Selected(); ok {
					value := f.options.SelectedKey()
					if _, known := f.selection[value]; !known {
						f.selectionOrder = append(f.selectionOrder, value)
					}
					f.selection[value] = !f.selection[value]
					f.filterOptions()
				}
				return nil
			}
		case "up", "down", "pgup", "pgdown", "home", "end":
			_, cmd := f.options.Update(msg)
			return cmd
		}
	}
	var cmd tea.Cmd
	f.query, cmd = f.query.Update(msg)
	f.filterOptions()
	return cmd
}

func (f *Form) View() string {
	if f.baseline == nil {
		f.Begin()
	}
	w, h := f.Width, f.Height
	if w <= 0 {
		w = 78
	}
	if h <= 0 {
		h = 20
	}
	if f.showError {
		return "错误详情\n" + f.errorScroll.View(f.Error, w, max(1, h-2)) + "\nEsc 返回字段 · PgUp/PgDn 滚动"
	}
	if f.discard.Active {
		f.discard.Width, f.discard.Height = w, h
		return f.discard.View()
	}
	if f.selecting {
		f.query.Width = max(1, w-10)
		f.options.Width, f.options.Height = w, max(1, h-3)
		hint := "↑/↓ 选择 · Enter 确认 · Esc 返回字段"
		if f.Fields[f.focus].Multi {
			hint = "Space 勾选 · Enter 确认多选 · Esc 放弃选择"
		}
		return styles.Title.Render("选择 "+f.Fields[f.focus].Label) + "\n搜索: " + f.query.View() + "\n" +
			f.options.View("没有匹配的选项；请修改搜索，或返回后添加可用对象。") + "\n" + styles.Dim.Render(hint)
	}
	var rows []string
	starts := make([]int, len(f.Fields))
	ends := make([]int, len(f.Fields))
	section := ""
	for i := range f.Fields {
		field := &f.Fields[i]
		if !f.visible(i) {
			continue
		}
		if field.Section != "" && field.Section != section {
			section = field.Section
			rows = append(rows, styles.Accent.Render("── "+section))
		}
		starts[i] = len(rows)
		prefix := "  "
		if i == f.focus {
			prefix = "> "
		}
		label := prefix + field.Label + ": "
		field.input.Width = max(4, w-DisplayWidth(label)-1)
		value := field.input.View()
		if field.Choice {
			value = field.Value()
			for _, opt := range field.Options {
				if opt.Value == value {
					value = opt.Label
					break
				}
			}
			if isBool(*field) {
				value = "[ ] 关闭"
				if field.Value() == "true" {
					value = "[x] 开启"
				}
			} else {
				value = "[" + value + "] Enter 选择"
			}
		}
		if field.Action != "" {
			value = "[条件行编辑器] Enter 打开 · " + field.Value()
		}
		if field.Multiline {
			field.area.SetWidth(max(1, w-2))
			field.area.SetHeight(max(2, min(5, h-10)))
			rows = append(rows, Clip(label, w))
			rows = append(rows, strings.Split(field.area.View(), "\n")...)
		} else {
			rows = append(rows, Clip(label+value, w))
		}
		ends[i] = len(rows)
	}
	save := f.SaveLabel
	if save == "" {
		save = "保存"
	}
	buttons := "  [" + save + "]    [取消]"
	if f.focus == len(f.Fields) {
		buttons = "> [" + save + "]    [取消]"
	}
	if f.focus == len(f.Fields)+1 {
		buttons = "  [" + save + "]  > [取消]"
	}
	if f.ExtraAction != "" {
		mark := "    "
		if f.focus == len(f.Fields)+2 {
			mark = "  > "
		}
		buttons += mark + "[" + f.ExtraLabel + "]"
	}
	info := ""
	if f.focus < len(f.Fields) {
		field := f.Fields[f.focus]
		info = field.Help
		if field.Secret {
			info = "Ctrl+R 显示/隐藏凭据 · " + info
		}
		if field.Multiline {
			info = "每行一条 · Ctrl+S 导入 · " + info
		}
	}
	info = Fit(Wrap(info, w), w, 2)
	errView := ""
	if f.Error != "" {
		errView = styles.Err.Render(Fit(Wrap("错误: "+f.Error, w), w, min(3, max(1, h/5)))) + "\n"
	}
	hint := "Tab 字段/按钮 · Ctrl+S " + save + " · Esc 取消 · Ctrl+E 错误详情"
	if f.ExtraAction != "" {
		hint = "Ctrl+S " + save + " · Ctrl+U " + f.ExtraLabel + " · Tab 字段/按钮 · Esc 取消"
	}
	footer := info + "\n" + errView + Clip(buttons, w) + "\n" + styles.Dim.Render(Clip(hint, w))
	viewH := max(1, h-1-len(strings.Split(footer, "\n")))
	if f.focus < len(f.Fields) {
		start := starts[f.focus]
		end := ends[f.focus]
		if start < f.offset {
			f.offset = start
		}
		if end > f.offset+viewH {
			f.offset = max(0, end-viewH)
		}
	}
	f.offset = max(0, min(f.offset, len(rows)-viewH))
	body := strings.Join(rows[f.offset:min(len(rows), f.offset+viewH)], "\n")
	title := f.Title
	if f.hasAdvanced() {
		advanced := "展开高级"
		if f.ShowAdvanced {
			advanced = "收起高级"
		}
		title += " · Ctrl+G " + advanced
	}
	return Clip(styles.Title.Render(title), w) + "\n" + Fit(body, w, viewH) + "\n" + footer
}
