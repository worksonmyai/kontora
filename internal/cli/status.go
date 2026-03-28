package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

type StatusOpts struct {
	ShowClosed bool
}

var StatusOrder = map[ticket.Status]int{
	ticket.StatusInProgress: 0,
	ticket.StatusTodo:       1,
	ticket.StatusPaused:     2,
	// custom statuses default to rank 3
	ticket.StatusOpen:      4,
	ticket.StatusDone:      5,
	ticket.StatusCancelled: 6,
}

func StatusRank(s ticket.Status) int {
	if r, ok := StatusOrder[s]; ok {
		return r
	}
	return 3
}

var StatusColor = map[ticket.Status]lipgloss.Color{
	ticket.StatusInProgress: lipgloss.Color("2"), // green
	ticket.StatusTodo:       lipgloss.Color("4"), // blue
	ticket.StatusPaused:     lipgloss.Color("3"), // yellow
}

type ticketRow struct {
	cells   []string
	kontora bool
}

// Status scans cfg.TicketsDir, parses ticket files, and prints a formatted table.
func Status(cfg *config.Config, all bool, w io.Writer, opts StatusOpts) error {
	dir := config.ExpandTilde(cfg.TicketsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(w, "No tickets.")
			return nil
		}
		return fmt.Errorf("reading tickets dir: %w", err)
	}

	var tickets []*ticket.Ticket
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		t, err := ticket.ParseFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		if !all && !t.Kontora {
			continue
		}
		tickets = append(tickets, t)
	}

	slices.SortFunc(tickets, func(a, b *ticket.Ticket) int {
		if ra, rb := StatusRank(a.Status), StatusRank(b.Status); ra != rb {
			return ra - rb
		}
		ta := ticketSortTime(a)
		tb := ticketSortTime(b)
		if c := tb.Compare(ta); c != 0 {
			return c
		}
		if at, bt := a.Title(), b.Title(); at != bt {
			return strings.Compare(at, bt)
		}
		return strings.Compare(a.ID, b.ID)
	})

	if len(tickets) == 0 {
		fmt.Fprintln(w, "No tickets.")
		return nil
	}

	var visible []*ticket.Ticket
	hasClosedTasks := false
	for _, t := range tickets {
		if t.Status == ticket.StatusDone || t.Status == ticket.StatusCancelled {
			hasClosedTasks = true
			if !opts.ShowClosed {
				continue
			}
		}
		visible = append(visible, t)
	}

	if len(visible) == 0 && hasClosedTasks {
		fmt.Fprintln(w, "No active tickets. Use --closed to show done/cancelled.")
		return nil
	}

	if len(visible) == 0 {
		fmt.Fprintln(w, "No tickets.")
		return nil
	}

	rows := buildRows(cfg, visible)
	fmt.Fprintln(w, renderTable(rows))
	return nil
}

func buildRows(cfg *config.Config, tickets []*ticket.Ticket) []ticketRow {
	rows := make([]ticketRow, 0, len(tickets))
	for _, t := range tickets {
		stage := t.Stage
		if stage == "" && t.Pipeline == "" && t.Kontora {
			stage = "standalone"
		} else if stage == "" {
			stage = "—"
		}

		agent := app.AgentForStage(cfg, t.Pipeline, t.Stage)
		if agent == "" && t.Kontora {
			agent = cfg.DefaultAgent
		} else if agent == "" {
			agent = "—"
		}

		rows = append(rows, ticketRow{
			cells:   []string{t.ID, string(t.Status), stage, agent, Duration(t), FormatTimestamp(t.StartedAt), FormatTimestamp(t.CompletedAt)},
			kontora: t.Kontora,
		})
	}
	return rows
}

func renderTable(rows []ticketRow) string {
	headers := []string{"ID", "STATUS", "STAGE", "AGENT", "DURATION", "STARTED", "FINISHED"}
	pad := lipgloss.NewStyle().PaddingRight(3)

	tbl := table.New().
		Headers(headers...).
		Border(lipgloss.HiddenBorder()).
		StyleFunc(func(row, col int) lipgloss.Style {
			base := pad
			if row == table.HeaderRow {
				return base.Bold(true).Faint(true)
			}
			if row >= 0 && row < len(rows) {
				r := rows[row]
				if !r.kontora {
					return base.Faint(true)
				}
				status := ticket.Status(r.cells[1])
				isDone := status == ticket.StatusDone || status == ticket.StatusCancelled
				if isDone {
					return base.Faint(true)
				}
				switch col {
				case 0: // ID
					return base.Faint(true)
				case 1: // STATUS
					if c, ok := StatusColor[status]; ok {
						return base.Foreground(c)
					}
				case 2: // STAGE
					return base.Foreground(lipgloss.Color("6")) // cyan
				case 4: // DURATION
					if status == ticket.StatusInProgress {
						return base.Foreground(lipgloss.Color("2")) // green
					}
				case 5, 6: // STARTED, FINISHED
					return base.Faint(true)
				}
			}
			return base
		})

	for _, r := range rows {
		tbl.Row(r.cells...)
	}

	return tbl.Render()
}

func Duration(t *ticket.Ticket) string {
	switch t.Status { //nolint:exhaustive
	case ticket.StatusInProgress:
		if t.StartedAt != nil {
			return FormatDuration(time.Since(*t.StartedAt))
		}
	case ticket.StatusDone:
		if t.StartedAt != nil && t.CompletedAt != nil {
			return FormatDuration(t.CompletedAt.Sub(*t.StartedAt))
		}
	}
	return "—"
}

func FormatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

func FormatTimestamp(t *time.Time) string {
	if t == nil {
		return "—"
	}
	now := time.Now()
	d := now.Sub(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		if t.Year() == now.Year() {
			return t.Format("Jan 02 15:04")
		}
		return t.Format("Jan 02 2006")
	}
}

func ticketSortTime(t *ticket.Ticket) time.Time {
	if t.Status == ticket.StatusInProgress && t.StartedAt != nil {
		return *t.StartedAt
	}
	return derefTime(t.Created)
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
