package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/worksonmyai/kontora/internal/web"
)

type formField int

const (
	fieldTitle formField = iota
	fieldPath
	fieldPipeline
	fieldAgent
	fieldCount
)

var fieldLabels = [fieldCount]string{
	"Title",
	"Path",
	"Pipeline",
	"Agent",
}

var fieldPlaceholders = [fieldCount]string{
	"ticket title (required)",
	"relative path to project (required)",
	"pipeline name",
	"agent name",
}

type createModel struct {
	fields  [fieldCount]textinput.Model
	focused formField
	width   int
	height  int
	err     error
	initCmd tea.Cmd
}

func (m *createModel) setSize(width, height int) {
	m.width = width
	m.height = height
	w := max(20, width-20)
	for i := range fieldCount {
		m.fields[i].Width = w
	}
}

func newCreateModel(cfg web.ConfigInfo, width, height int) createModel {
	var fields [fieldCount]textinput.Model
	for i := range fieldCount {
		ti := textinput.New()
		ti.Prompt = "  "
		ti.Placeholder = fieldPlaceholders[i]
		ti.Width = max(20, width-20)
		// Rebind AcceptSuggestion to Right so Tab is free for field navigation.
		ti.KeyMap.AcceptSuggestion = key.NewBinding(key.WithKeys("right"))
		ti.ShowSuggestions = true
		fields[i] = ti
	}

	if len(cfg.Pipelines) > 0 {
		fields[fieldPipeline].SetSuggestions(cfg.Pipelines)
	}
	if len(cfg.Agents) > 0 {
		fields[fieldAgent].SetSuggestions(cfg.Agents)
	}

	cmd := fields[fieldTitle].Focus()

	return createModel{
		fields:  fields,
		focused: fieldTitle,
		width:   width,
		height:  height,
		initCmd: cmd,
	}
}

func (m createModel) Update(msg tea.Msg) (createModel, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		// Forward non-key messages to focused field.
		var cmd tea.Cmd
		m.fields[m.focused], cmd = m.fields[m.focused].Update(msg)
		return m, cmd
	}

	switch {
	case isKey(keyMsg, "tab"):
		m.fields[m.focused].Blur()
		m.focused = (m.focused + 1) % fieldCount
		cmd := m.fields[m.focused].Focus()
		return m, cmd
	case isKey(keyMsg, "shift+tab"):
		m.fields[m.focused].Blur()
		m.focused = (m.focused - 1 + fieldCount) % fieldCount
		cmd := m.fields[m.focused].Focus()
		return m, cmd
	default:
		var cmd tea.Cmd
		m.fields[m.focused], cmd = m.fields[m.focused].Update(msg)
		return m, cmd
	}
}

func (m createModel) View() string {
	var b strings.Builder

	b.WriteString(styleHeader.Render(" new ticket"))
	b.WriteByte('\n')
	b.WriteString(styleFaint.Render(strings.Repeat("─", m.width)))
	b.WriteByte('\n')

	for i := range fieldCount {
		label := fieldLabels[i]
		if i == m.focused {
			label = styleColHeaderActive.Render(fmt.Sprintf("  %s", label))
		} else {
			label = styleFaint.Render(fmt.Sprintf("  %s", label))
		}
		b.WriteString(label)
		b.WriteByte('\n')
		b.WriteString(m.fields[i].View())
		b.WriteByte('\n')
	}

	if m.err != nil {
		b.WriteString("\n  " + styleError.Render(m.err.Error()))
		b.WriteByte('\n')
	}

	// Fill remaining height.
	rendered := strings.Count(b.String(), "\n")
	for range m.height - rendered - 2 {
		b.WriteByte('\n')
	}

	b.WriteByte('\n')
	b.WriteString(styledHints([]string{"tab next", "enter create", "esc cancel"}))

	return b.String()
}

func (m createModel) validate() error {
	if strings.TrimSpace(m.fields[fieldTitle].Value()) == "" {
		return fmt.Errorf("title is required")
	}
	if strings.TrimSpace(m.fields[fieldPath].Value()) == "" {
		return fmt.Errorf("path is required")
	}
	return nil
}

func (m createModel) request() web.CreateTicketRequest {
	return web.CreateTicketRequest{
		Title:    strings.TrimSpace(m.fields[fieldTitle].Value()),
		Path:     strings.TrimSpace(m.fields[fieldPath].Value()),
		Pipeline: strings.TrimSpace(m.fields[fieldPipeline].Value()),
		Agent:    strings.TrimSpace(m.fields[fieldAgent].Value()),
		Status:   "todo",
	}
}
