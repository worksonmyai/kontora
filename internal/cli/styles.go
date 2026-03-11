package cli

import "github.com/charmbracelet/lipgloss"

var (
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // yellow
	styleFail  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	styleBold  = lipgloss.NewStyle().Bold(true)
	styleFaint = lipgloss.NewStyle().Faint(true)
	styleCyan  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)
