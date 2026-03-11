package tui

import (
	"slices"

	tea "github.com/charmbracelet/bubbletea"
)

func isKey(msg tea.KeyMsg, keys ...string) bool {
	if slices.Contains(keys, msg.String()) {
		return true
	}
	if msg.Type == tea.KeyEscape && slices.Contains(keys, "esc") {
		return true
	}
	return false
}
