package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
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

// StageActivity returns the structured activity tape for one run of a stage.
// When that run has no sidecar it returns tape == nil and the shared plaintext
// <stage>.log instead, which is the permanent case for agents that write no
// session JSONL.
//
// Unlike Logs, a named stage with no log of its own is os.ErrNotExist rather
// than the newest log in the directory: this response names the stage and run
// it describes, so returning another stage's bytes would mislabel them. An
// empty stage keeps the newest-log fallback.
func StageActivity(tasksDir, logsDir, taskID, stage string, run int) (tape *logfmt.Tape, content string, err error) {
	tasksDir = config.ExpandTilde(tasksDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return nil, "", err
	}
	logDir := filepath.Join(config.ExpandTilde(logsDir), resolvedID)

	if stage == "" {
		var buf bytes.Buffer
		if err := Logs(tasksDir, logsDir, resolvedID, "", &buf); err != nil {
			return nil, "", err
		}
		return nil, buf.String(), nil
	}

	sidecar := filepath.Join(logDir, fmt.Sprintf("%s.%d.events.json", stage, run))
	switch data, readErr := os.ReadFile(sidecar); {
	case readErr == nil:
		var t logfmt.Tape
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, "", fmt.Errorf("parsing activity sidecar %s: %w", sidecar, err)
		}
		return &t, "", nil
	case !errors.Is(readErr, os.ErrNotExist):
		return nil, "", readErr
	}

	data, err := os.ReadFile(filepath.Join(logDir, stage+".log"))
	if err != nil {
		return nil, "", err
	}
	return nil, string(data), nil
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
