package cli

import (
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// ResolveSchedule turns the --at and --after flags into the value that is
// stored. Exactly one of them may be given; neither returns "", which the
// callers read as "no schedule was asked for".
//
// Both flags take the spellings a person types — an offset or a local wall
// time for --at, days and weeks for --after — and hand back the one spelling
// the field stores.
//
// --after is resolved here rather than on the daemon, so the instant is
// measured from when the command was typed and does not drift with the round
// trip.
func ResolveSchedule(at, after string, now time.Time) (string, error) {
	switch {
	case at != "" && after != "":
		return "", fmt.Errorf("--at and --after name the same thing; give one of them")
	case at != "":
		parsed, err := ticket.ParseScheduleFlex(at)
		if err != nil {
			return "", err
		}
		// A mistyped year is the failure this catches: without it the ticket
		// runs at once, and the confirmation reads like a schedule was set.
		if ticket.SchedulePast(parsed, now) {
			return "", fmt.Errorf("--at is in the past: %s", ticket.FormatSchedule(parsed))
		}
		return ticket.FormatSchedule(parsed), nil
	case after != "":
		d, err := ticket.ParseScheduleDelay(after)
		if err != nil {
			return "", fmt.Errorf("--after %w", err)
		}
		return ticket.FormatSchedule(now.Add(d)), nil
	default:
		return "", nil
	}
}

// Schedule sets or clears a ticket's pickup time through the local daemon. Like
// Run, it goes through the daemon's HTTP API rather than writing the file,
// because the daemon has to arm its timer and keep the write from racing a
// pickup.
func Schedule(cfg *config.Config, taskID string, req web.ScheduleTicketRequest) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}
	return LocalClient(cfg).Schedule(resolvedID, req)
}
