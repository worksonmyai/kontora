package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/worksonmyai/kontora/internal/ticket"
)

var (
	styleHeader            = lipgloss.NewStyle().Bold(true)
	styleFaint             = lipgloss.NewStyle().Faint(true)
	styleSelected          = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleFilter            = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleActiveTab         = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("6"))
	styleTab               = lipgloss.NewStyle().Faint(true)
	styleCyan              = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleColHeaderActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleColHeaderInactive = lipgloss.NewStyle().Faint(true)
	styleError             = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleBrand             = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
	styleConnected         = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleDisconnected      = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleRunning           = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleCardTitle         = lipgloss.NewStyle().Bold(true)
	styleKey               = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))

	statusStyle = map[ticket.Status]lipgloss.Style{
		ticket.StatusInProgress: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		ticket.StatusTodo:       lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
		ticket.StatusPaused:     lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		ticket.StatusDone:       lipgloss.NewStyle().Faint(true),
		ticket.StatusCancelled:  lipgloss.NewStyle().Faint(true).Foreground(lipgloss.Color("1")),
		ticket.StatusOpen:       lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
	}
)

var styleCustomStatus = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))

func styledStatus(s string) string {
	st, ok := statusStyle[ticket.Status(s)]
	if !ok {
		return styleCustomStatus.Render(s)
	}
	return st.Render(s)
}

// styledHint renders a status bar hint like "q quit" with the key colored.
func styledHint(hint string) string {
	k, rest, ok := strings.Cut(hint, " ")
	if !ok {
		return hint
	}
	return styleKey.Render(k) + " " + styleFaint.Render(rest)
}

func styledHints(hints []string) string {
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = styledHint(h)
	}
	return " " + strings.Join(parts, styleFaint.Render(" · "))
}
