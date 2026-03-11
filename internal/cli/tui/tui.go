package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/tmux"
)

// Run starts the interactive TUI. It auto-detects the data source
// (daemon API or file-based) and launches a fullscreen Bubble Tea program.
func Run(cfg *config.Config) error {
	src := newSource(cfg)

	m := newModel(src)
	m.list.connected = src.Connected()

	p := tea.NewProgram(m, tea.WithAltScreen())

	if src.Connected() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := src.Subscribe(ctx)
		if ch != nil {
			go func() {
				for ev := range ch {
					p.Send(taskUpdatedMsg(ev))
				}
			}()
		}
	}

	result, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI: %w", err)
	}

	if final, ok := result.(model); ok && final.attachTarget != "" {
		return doAttach(final.attachTarget)
	}
	return nil
}

func doAttach(taskID string) error {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	target := tmux.WindowTarget(tmux.DefaultSessionName, taskID)
	args := []string{"tmux", "attach-session", "-t", target, "-r"}
	return syscall.Exec(tmuxBin, args, os.Environ())
}
