package cli

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/worksonmyai/kontora/internal/ticket"
)

var StatusOrder = map[ticket.Status]int{
	ticket.StatusInProgress: 0,
	ticket.StatusTodo:       1,
	ticket.StatusPaused:     2,
	// custom statuses default to rank 3
	ticket.StatusOpen:      4,
	ticket.StatusDone:      5,
	ticket.StatusCancelled: 6,
	// Legacy closed ranks with the other closed statuses. Without an entry it
	// would take the custom-status rank and sort above open.
	ticket.StatusLegacyClosed: 6,
	ticket.StatusArchived:     7,
}

// StatusRank orders the status column of a listing and of the board.
func StatusRank(s ticket.Status) int {
	if r, ok := StatusOrder[s]; ok {
		return r
	}
	return 3
}

var StatusColor = map[ticket.Status]lipgloss.Color{
	ticket.StatusInProgress:  lipgloss.Color("2"),  // green
	ticket.StatusTodo:        lipgloss.Color("4"),  // blue
	ticket.StatusPaused:      lipgloss.Color("3"),  // yellow
	ticket.StatusHumanReview: lipgloss.Color("13"), // magenta
	"review":                 lipgloss.Color("13"), // magenta
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

// FormatSchedule renders a stored scheduled_at for a person: the instant in the
// reader's own zone. A value the parser rejects is printed as it stands, so a
// hand-edited typo is visible rather than shown as a wrong time.
//
// The year is printed whenever it is not the current one. A schedule is the one
// field whose whole point is a long horizon, so hiding the year would hide the
// mistyped year that is the easiest way to get one wrong.
func FormatSchedule(scheduledAt string) string {
	at, err := ticket.ParseSchedule(scheduledAt)
	if err != nil {
		return scheduledAt
	}
	local := at.Local()
	if local.Year() != time.Now().Year() {
		return local.Format("Jan 02 2006 15:04")
	}
	return local.Format("Jan 02 15:04")
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
