package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/tmux"
)

// ErrCancelled is returned when the user cancels an interactive picker.
var ErrCancelled = errors.New("cancelled")

// Attach connects to the tmux window of a running ticket. When ticketID is empty,
// an interactive Bubble Tea picker is shown. Attachments are read-only unless
// readWrite is true.
func Attach(cfg *config.Config, taskID string, readWrite bool) error {
	if taskID == "" {
		return attachInteractive(cfg, readWrite)
	}
	target, err := resolveAttach(cfg.TicketsDir, taskID)
	if err != nil {
		return err
	}
	return execAttach(target, readWrite)
}

func attachInteractive(cfg *config.Config, readWrite bool) error {
	target, rw, err := pickSession(cfg, readWrite)
	if err != nil {
		return err
	}
	return execAttach(target, rw)
}

// execAttach replaces the current process with tmux attach-session.
// It passes -r (read-only) unless readWrite is true.
func execAttach(target string, readWrite bool) error {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		return fmt.Errorf("tmux not found: %w", err)
	}

	args := []string{"tmux", "attach-session", "-t", target}
	if !readWrite {
		args = append(args, "-r")
	}
	return syscall.Exec(tmuxBin, args, os.Environ())
}

// resolveAttach validates that the ticket exists, is running, and has a tmux
// window. Returns the window target to attach to.
func resolveAttach(tasksDir, taskID string) (string, error) {
	tasksDir = config.ExpandTilde(tasksDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return "", err
	}

	t, err := ticket.ParseFile(filepath.Join(tasksDir, resolvedID+".md"))
	if err != nil {
		return "", fmt.Errorf("reading ticket: %w", err)
	}

	if t.Status != ticket.StatusInProgress {
		return "", fmt.Errorf("ticket %s has status %q, must be in_progress to attach", resolvedID, t.Status)
	}

	if !tmux.HasWindow(tmux.DefaultSessionName, resolvedID) {
		return "", fmt.Errorf("tmux window %q not found for ticket %s", tmux.WindowTarget(tmux.DefaultSessionName, resolvedID), resolvedID)
	}

	return tmux.WindowTarget(tmux.DefaultSessionName, resolvedID), nil
}

// attachItem represents a running kontora tmux window with optional ticket metadata.
type attachItem struct {
	id     string
	target string
	title  string
	stage  string
	agent  string
	dur    string
}

// attachModel is the Bubble Tea model for the session picker.
type attachModel struct {
	items     []attachItem
	cursor    int
	readWrite bool
	cancelled bool
}

func (m attachModel) Init() tea.Cmd { return nil }

func (m attachModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok {
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.items)-1 {
				m.cursor++
			}
		case "tab":
			m.readWrite = !m.readWrite
		case "enter":
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

var (
	styleSel = lipgloss.NewStyle().Bold(true)
	styleDim = lipgloss.NewStyle().Faint(true)
	styleRO  = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	styleRW  = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
)

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

func (m attachModel) View() string {
	var b strings.Builder
	b.WriteString("Select session to attach:\n\n")

	for i, item := range m.items {
		line := fmt.Sprintf("%-10s %-42s %-10s %-16s %s",
			item.id, truncate(item.title, 40), truncate(item.stage, 10), truncate(item.agent, 16), item.dur)

		if i == m.cursor {
			fmt.Fprintf(&b, "▸ %s\n", styleSel.Render(line))
		} else {
			fmt.Fprintf(&b, "  %s\n", styleDim.Render(line))
		}
	}

	b.WriteByte('\n')
	if m.readWrite {
		b.WriteString(styleRW.Render("[RW]"))
	} else {
		b.WriteString(styleRO.Render("[RO]"))
	}
	b.WriteString("  j/k navigate · enter attach · tab toggle RO/RW · q cancel\n")

	return b.String()
}

// pickSession shows a TUI picker of running kontora tmux windows.
// Returns the selected window target and readWrite preference.
func pickSession(cfg *config.Config, readWrite bool) (string, bool, error) {
	items, err := buildAttachItems(cfg)
	if err != nil {
		return "", false, err
	}
	if len(items) == 0 {
		return "", false, fmt.Errorf("no running kontora windows")
	}

	m := attachModel{
		items:     items,
		readWrite: readWrite,
	}
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", false, fmt.Errorf("picker: %w", err)
	}
	final := result.(attachModel)
	if final.cancelled {
		return "", false, ErrCancelled
	}
	selected := final.items[final.cursor]
	return selected.target, final.readWrite, nil
}

// buildAttachItems cross-references tmux windows with ticket files.
func buildAttachItems(cfg *config.Config) ([]attachItem, error) {
	windows, err := tmux.ListWindows(tmux.DefaultSessionName)
	if err != nil {
		return nil, err
	}

	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	tasksByID := make(map[string]*ticket.Ticket)
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return nil, fmt.Errorf("reading tickets dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		t, err := ticket.ParseFile(filepath.Join(tasksDir, entry.Name()))
		if err != nil {
			continue
		}
		if t.Status == ticket.StatusInProgress {
			tasksByID[t.ID] = t
		}
	}

	var items []attachItem
	for _, windowName := range windows {
		item := attachItem{
			target: tmux.WindowTarget(tmux.DefaultSessionName, windowName),
			id:     windowName,
			title:  "—",
			stage:  "—",
			agent:  "—",
			dur:    "—",
		}

		if t, ok := tasksByID[windowName]; ok {
			item.title = t.Title()
			if item.title == "" {
				item.title = "—"
			}
			if t.Role != "" {
				item.stage = t.Role
			}
			agent := app.AgentForStage(cfg, t.Pipeline, t.Role)
			if agent != "" {
				item.agent = agent
			}
			if t.StartedAt != nil {
				item.dur = FormatDuration(time.Since(*t.StartedAt))
			}
		}

		items = append(items, item)
	}
	return items, nil
}
