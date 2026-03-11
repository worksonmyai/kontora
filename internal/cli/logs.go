package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// Logs prints the agent log for a ticket. If stage is empty, it shows the most
// recent log file by modification time. Falls back to ticket history entries
// when no log files exist.
func Logs(tasksDir, logsDir, taskID, stage string, w io.Writer) error {
	tasksDir = config.ExpandTilde(tasksDir)
	logsDir = config.ExpandTilde(logsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	logDir := filepath.Join(logsDir, resolvedID)
	if stage != "" {
		err := printFile(filepath.Join(logDir, stage+".log"), w)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Stage-specific log not found — fall through to newest/history.
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		return printTaskHistory(tasksDir, resolvedID, w)
	}

	var newest string
	var newestTime int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().UnixNano() > newestTime {
			newestTime = info.ModTime().UnixNano()
			newest = entry.Name()
		}
	}

	if newest == "" {
		return printTaskHistory(tasksDir, resolvedID, w)
	}

	return printFile(filepath.Join(logDir, newest), w)
}

func resolveTaskID(tasksDir, input string) (string, error) {
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return "", fmt.Errorf("reading tickets dir: %w", err)
	}

	var prefixMatch string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		if name == input {
			return input, nil
		}
		if prefixMatch == "" && strings.HasPrefix(name, input) {
			prefixMatch = name
		}
	}

	if prefixMatch != "" {
		return prefixMatch, nil
	}
	return "", fmt.Errorf("ticket %q not found", input)
}

func printFile(path string, w io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading log: %w", err)
	}
	_, err = w.Write(data)
	return err
}

func printTaskHistory(tasksDir, taskID string, w io.Writer) error {
	t, err := ticket.ParseFile(filepath.Join(tasksDir, taskID+".md"))
	if err != nil {
		fmt.Fprintln(w, "no logs found")
		return nil //nolint:nilerr // intentional: missing/unparseable ticket file means no history to show
	}

	if len(t.History) == 0 {
		fmt.Fprintln(w, styleFaint.Render("no logs found"))
		return nil
	}

	pad := lipgloss.NewStyle().PaddingRight(2)
	headers := []string{"STAGE", "AGENT", "EXIT", "STARTED", "COMPLETED"}
	tbl := table.New().
		Headers(headers...).
		Border(lipgloss.HiddenBorder()).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return pad.Bold(true).Faint(true)
			}
			if col == 2 { // EXIT column
				return pad
			}
			return pad
		})

	for _, h := range t.History {
		started := "—"
		if h.StartedAt != nil {
			started = h.StartedAt.Format("2006-01-02 15:04:05")
		}
		completed := "—"
		if h.CompletedAt != nil {
			completed = h.CompletedAt.Format("2006-01-02 15:04:05")
		}
		exitCode := fmt.Sprintf("%d", h.ExitCode)
		tbl.Row(h.Stage, h.Agent, exitCode, started, completed)
	}

	fmt.Fprintln(w, tbl.Render())
	return nil
}
