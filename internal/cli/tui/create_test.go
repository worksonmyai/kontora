package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/worksonmyai/kontora/internal/web"
)

func TestCreateModel_FieldNavigation(t *testing.T) {
	m := newCreateModel(web.ConfigInfo{}, 80, 24)

	assert.Equal(t, fieldTitle, m.focused)

	// Tab forward through all fields
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, fieldPath, m.focused)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, fieldPipeline, m.focused)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, fieldAgent, m.focused)

	// Tab wraps around
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	assert.Equal(t, fieldTitle, m.focused)

	// Shift-Tab goes back
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	assert.Equal(t, fieldAgent, m.focused)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	assert.Equal(t, fieldPipeline, m.focused)
}

func TestCreateModel_Validate(t *testing.T) {
	tests := []struct {
		name    string
		title   string
		path    string
		wantErr bool
	}{
		{"empty title", "", "some/path", true},
		{"empty path", "My Ticket", "", true},
		{"both empty", "", "", true},
		{"whitespace only", "  ", "  ", true},
		{"valid", "My Ticket", "some/path", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newCreateModel(web.ConfigInfo{}, 80, 24)
			m.fields[fieldTitle].SetValue(tc.title)
			m.fields[fieldPath].SetValue(tc.path)
			err := m.validate()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCreateModel_Request(t *testing.T) {
	cfg := web.ConfigInfo{
		Pipelines: []string{"default", "review"},
		Agents:    []string{"claude", "aider"},
	}
	m := newCreateModel(cfg, 80, 24)
	m.fields[fieldTitle].SetValue("My Ticket")
	m.fields[fieldPath].SetValue("./myproject")
	m.fields[fieldPipeline].SetValue("default")
	m.fields[fieldAgent].SetValue("claude")

	req := m.request()
	assert.Equal(t, "My Ticket", req.Title)
	assert.Equal(t, "./myproject", req.Path)
	assert.Equal(t, "default", req.Pipeline)
	assert.Equal(t, "claude", req.Agent)
	assert.Equal(t, "todo", req.Status)
}
