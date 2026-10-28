package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

type Field struct {
	Name      string
	Label     string
	Required  bool
	Multiline bool
	Input     textinput.Model
	Area      textarea.Model
	Error     string
	Choices   []Choice
	Section   string
	When      func(map[string]string) bool
	Static    bool
}

func ChoiceField(name, label, value string, choices ...Choice) Field {
	f := TextField(name, label, value, false, false)
	f.Choices = choices
	return f
}

func (f Field) choiceLabel() string {
	for _, c := range f.Choices {
		if c.ID == f.Value() {
			return c.Label
		}
	}
	return f.Value()
}

func TextField(name, label, value string, required, secret bool) Field {
	f := Field{Name: name, Label: label, Required: required, Input: textinput.New()}
	f.Input.SetValue(value)
	f.Input.CharLimit = 4096
	if secret {
		f.Input.EchoMode = textinput.EchoPassword
		f.Input.EchoCharacter = '*'
	}
	return f
}
func MultilineField(name, label, value string, required bool) Field {
	f := Field{Name: name, Label: label, Required: required, Multiline: true, Area: textarea.New()}
	f.Area.SetValue(value)
	f.Area.ShowLineNumbers = false
	f.Area.SetHeight(4)
	f.Area.CharLimit = 1 << 20
	f.Area.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"))
	return f
}
func (f Field) Value() string {
	if f.Multiline {
		return f.Area.Value()
	}
	return f.Input.Value()
}
func (f *Field) blur() {
	if f.Multiline {
		f.Area.Blur()
	} else {
		f.Input.Blur()
	}
}
func (f *Field) focus() tea.Cmd {
	if f.Static || len(f.Choices) > 0 {
		return nil
	}
	if f.Multiline {
		return f.Area.Focus()
	}
	return f.Input.Focus()
}
func (f *Field) update(msg tea.Msg) tea.Cmd {
	if f.Static {
		return nil
	}
	if len(f.Choices) > 0 {
		if key, ok := msg.(tea.KeyPressMsg); ok {
			delta := 0
			switch key.String() {
			case "left":
				delta = -1
			case "right", " ", "space":
				delta = 1
			}
			if delta != 0 {
				index := 0
				for i, c := range f.Choices {
					if c.ID == f.Value() {
						index = i
						break
					}
				}
				for n := 0; n < len(f.Choices); n++ {
					index = (index + delta + len(f.Choices)) % len(f.Choices)
					if !f.Choices[index].Disabled {
						f.Input.SetValue(f.Choices[index].ID)
						break
					}
				}
			}
		}
		return nil
	}
	var cmd tea.Cmd
	if paste, ok := msg.(tea.PasteMsg); ok && f.Multiline {
		f.Area.InsertString(paste.Content)
		return nil
	}
	if f.Multiline {
		f.Area, cmd = f.Area.Update(msg)
	} else {
		f.Input, cmd = f.Input.Update(msg)
	}
	return cmd
}

// Form owns widget focus and submission routing; widgets never decide whether
// pasted multiline data, Enter or Tab is a business submission.
type Form struct {
	Fields   []Field
	Index    int
	Busy     bool
	initial  []string
	Sections []Choice
	Section  string
}

func NewForm(fields ...Field) *Form {
	f := &Form{Fields: fields}
	for _, field := range fields {
		f.initial = append(f.initial, field.Value())
	}
	return f
}
func (f *Form) Focus(index int) tea.Cmd {
	for i := range f.Fields {
		f.Fields[i].blur()
	}
	f.Index = max(0, min(index, len(f.Fields)-1))
	if len(f.Fields) == 0 {
		f.Index = -1
		return nil
	}
	if f.Fields[f.Index].Section != "" {
		f.Section = f.Fields[f.Index].Section
	}
	return f.Fields[f.Index].focus()
}

func (f *Form) visible(index int) bool {
	field := f.Fields[index]
	return field.When == nil || field.When(f.Values())
}

func (f *Form) next(delta int) int {
	for i := f.Index + delta; i >= 0 && i < len(f.Fields); i += delta {
		if f.visible(i) {
			return i
		}
	}
	return -1
}

func (f *Form) switchSection(delta int) tea.Cmd {
	index := 0
	for i, s := range f.Sections {
		if s.ID == f.Section {
			index = i
		}
	}
	if len(f.Sections) == 0 {
		return nil
	}
	f.Section = f.Sections[(index+delta+len(f.Sections))%len(f.Sections)].ID
	for i, field := range f.Fields {
		if field.Section == f.Section && f.visible(i) {
			return f.Focus(i)
		}
	}
	return nil
}

func (f *Form) update(msg tea.Msg) tea.Cmd {
	if f.Busy || f.Index < 0 || f.Index >= len(f.Fields) {
		return nil
	}
	return f.Fields[f.Index].update(msg)
}
func (f *Form) Dirty() bool {
	if len(f.Fields) != len(f.initial) {
		return true
	}
	for i, field := range f.Fields {
		if field.Value() != f.initial[i] {
			return true
		}
	}
	return false
}
func (f *Form) Values() map[string]string {
	values := make(map[string]string, len(f.Fields))
	for _, field := range f.Fields {
		values[field.Name] = field.Value()
	}
	return values
}
func (f *Form) Validate(requiredMessage string) tea.Cmd {
	first := -1
	for i := range f.Fields {
		field := &f.Fields[i]
		field.Error = ""
		if f.visible(i) && field.Required && strings.TrimSpace(field.Value()) == "" {
			field.Error = requiredMessage
			if first == -1 {
				first = i
			}
		}
	}
	if first != -1 {
		return f.Focus(first)
	}
	return nil
}
func (f *Form) Invalid() bool {
	for _, field := range f.Fields {
		if field.Error != "" {
			return true
		}
	}
	return false
}
func (f *Form) Resize(width int) {
	for i := range f.Fields {
		if f.Fields[i].Multiline {
			f.Fields[i].Area.SetWidth(max(1, width))
		} else {
			f.Fields[i].Input.SetWidth(max(1, width))
		}
	}
}
