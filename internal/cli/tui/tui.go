package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		session, socket := src.Attachment()
		return doAttach(session, socket, final.attachTarget)
	}
	return nil
}

func doAttach(sessionName, socket, taskID string) error {
	args, err := attachArgs(sessionName, socket, taskID)
	if err != nil {
		return err
	}
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}
	return syscall.Exec(tmuxBin, args, os.Environ())
}

func attachArgs(sessionName, socket, taskID string) ([]string, error) {
	if sessionName == "" || !filepath.IsAbs(socket) || taskID == "" {
		return nil, errAttachmentUnavailable
	}
	target := tmux.WindowTarget(sessionName, taskID)
	return []string{"tmux", "-S", socket, "attach-session", "-t", target, "-r"}, nil
}
