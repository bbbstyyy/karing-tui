package pages

import (
	"fmt"
	"github.com/bbbstyyy/karing-tui/internal/tui/keys"
	"strconv"
	"strings"

	"github.com/bbbstyyy/karing-tui/internal/application"
	"github.com/bbbstyyy/karing-tui/internal/config"
	"github.com/bbbstyyy/karing-tui/internal/tui/components"
	tea "github.com/charmbracelet/bubbletea"
)

// logicalEditor edits a private copy; only its Save action updates the parent
// rule form. Each condition has its own type, value and NOT switch.
type logicalEditor struct {
	app           *application.App
	mode          string
	conditions    []config.RuleCondition
	baseline      string
	list          components.SimpleList
	form          *components.Form
	edit          int
	err           error
	confirm       components.Confirm
	width, height int
}

func newLogicalEditor(app *application.App, expr string) *logicalEditor {
	e := &logicalEditor{app: app, mode: "and"}
	if expr != "" {
		mode, conditions, err := config.ParseLogicalExpr(expr)
		if err == nil {
			e.mode, e.conditions = mode, conditions
		} else {
			e.err = err
		}
	}
	e.baseline = e.value()
	e.reload()
	return e
}

func (e *logicalEditor) value() string { return config.FormatLogicalExpr(e.mode, e.conditions) }
func (e *logicalEditor) dirty() bool {
	return e.value() != e.baseline || (e.form != nil && e.form.Dirty())
}

func (e *logicalEditor) reload() {
	var rows [][]string
	var ids []string
	for i, c := range e.conditions {
		not := "否"
		if c.Invert {
			not = "NOT"
		}
		rows = append(rows, []string{c.Value, c.Type, not})
		ids = append(ids, strconv.Itoa(i))
	}
	e.list.SetTable([]components.Column{col("条件值", 20, 0, false), col("类型", 15, 0, false), col("取反", 4, 0, false)}, rows, ids)
}

func (e *logicalEditor) open(index int) {
	e.edit = index
	f := components.NewForm("编辑逻辑条件", []string{"类型", "值", "NOT 取反"}, []string{"type", "value", "invert"}, []string{"选择匹配类型", "", "当前条件不满足时匹配"})
	f.SetChoices("type", ruleTypeChoices(false))
	if index >= 0 {
		c := e.conditions[index]
		f.SetValueByKey("type", c.Type)
		f.SetValueByKey("value", c.Value)
		f.SetValueByKey("invert", strconv.FormatBool(c.Invert))
	}
	configureRuleValue(e.app, &f)
	f.Begin()
	e.form = &f
	e.err = nil
}

func (e *logicalEditor) update(msg tea.Msg) (string, tea.Cmd) {
	if e.form != nil {
		previousType := e.form.ValueByKey("type")
		action, cmd := e.form.Handle(msg)
		syncRuleValueOnTypeChange(e.app, e.form, previousType)
		if action == "cancel" {
			e.form = nil
			e.err = nil
		}
		if action == "save" {
			c := config.RuleCondition{Type: e.form.ValueByKey("type"), Value: e.form.ValueByKey("value"), Invert: e.form.ValueByKey("invert") == "true"}
			if err := e.app.Rout.ValidateCondition(c); err != nil {
				e.err = err
				e.form.SetError(err)
				return "", cmd
			}
			if e.edit < 0 {
				e.conditions = append(e.conditions, c)
			} else {
				e.conditions[e.edit] = c
			}
			e.form = nil
			e.err = nil
			e.reload()
			if e.edit < 0 {
				e.list.Cursor = len(e.conditions) - 1
			}
		}
		return "", cmd
	}
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return "", nil
	}
	if e.confirm.Active {
		if done, accept := e.confirm.Choose(key.String()); done && accept {
			if e.confirm.ID == "discard" {
				return "cancel", nil
			}
			idx := e.list.Cursor
			e.conditions = append(e.conditions[:idx], e.conditions[idx+1:]...)
			e.reload()
		}
		return "", nil
	}
	if consumed, cmd := e.list.Update(msg); consumed {
		return "", cmd
	}
	switch key.String() {
	case "a":
		e.open(-1)
	case keys.Enter, "e":
		if e.list.Cursor < len(e.conditions) {
			e.open(e.list.Cursor)
		}
	case "d":
		if e.list.Cursor < len(e.conditions) {
			e.confirm = components.NewConfirm("delete", "删除当前逻辑条件？仅保存父规则后生效。")
		}
	case " ":
		if e.list.Cursor < len(e.conditions) {
			e.conditions[e.list.Cursor].Invert = !e.conditions[e.list.Cursor].Invert
			e.reload()
		}
	case "[", "]":
		if e.mode == "and" {
			e.mode = "or"
		} else {
			e.mode = "and"
		}
	case keys.Save:
		if len(e.conditions) == 0 {
			e.err = fmt.Errorf("至少添加一个条件，按 a 添加")
		} else {
			return "save", nil
		}
	case keys.Cancel, "q":
		if e.dirty() {
			e.confirm = components.NewConfirm("discard", "放弃本次条件修改并返回规则表单？")
		} else {
			return "cancel", nil
		}
	}
	return "", nil
}

func (e *logicalEditor) view(width, height int) string {
	e.width, e.height = width, height
	if e.form != nil {
		e.form.Width, e.form.Height = width, height
		return e.form.View()
	}
	if e.confirm.Active {
		e.confirm.Width, e.confirm.Height = width, height
		return e.confirm.View()
	}
	mode := "[AND 全部满足]  OR 任一满足"
	if e.mode == "or" {
		mode = "AND 全部满足  [OR 任一满足]"
	}
	foot := "a 添加 · e 编辑 · d 删除 · Space NOT · Ctrl+S 确认 · Esc 返回"
	if e.err != nil {
		foot = components.Clip("错误: "+e.err.Error(), width) + "\n" + foot
	}
	head := components.Wrap("逻辑条件 / "+mode+" · [/] 切换", width)
	foot = components.Wrap(foot, width)
	e.list.Width, e.list.Height = width, max(1, height-len(strings.Split(head, "\n"))-len(strings.Split(foot, "\n")))
	return head + "\n" + e.list.View("暂无条件，按 a 添加第一行。") + "\n" + foot
}
