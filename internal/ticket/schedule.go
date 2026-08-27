package ticket

import (
	"fmt"
	"time"
)

// FieldScheduledAt is the frontmatter key holding a ticket's pickup time.
const FieldScheduledAt = "scheduled_at"

// ParseSchedule reads a scheduled_at value. It is deliberately stricter than
// the YAML timestamp resolver, which also accepts a space between the date and
// the time, a missing zone, and fractional forms the resolver normalizes away:
// the field is stored and compared as text, so exactly one spelling may reach
// it.
func ParseSchedule(s string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 timestamp such as 2026-09-01T09:00:00Z, got %q", FieldScheduledAt, s)
	}
	return at, nil
}

// FormatSchedule renders an instant the way it is stored: UTC, second
// precision. An offset the caller passed in survives as the same instant, so a
// schedule set from two time zones compares equal.
func FormatSchedule(at time.Time) string {
	return at.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// ScheduleGrace is how far into the past a schedule may be set. An --at value
// is resolved before the round trip that stores it, so an instant a second old
// was current when it was typed. A year-old one is a typo, and the point of a
// schedule is not to start the agent at once.
const ScheduleGrace = time.Minute

// SchedulePast reports whether at is too far behind now to be a pickup time
// anyone asked for.
func SchedulePast(at, now time.Time) bool {
	return at.Before(now.Add(-ScheduleGrace))
}

// Schedule returns the ticket's pickup time. The second result is false when
// the ticket carries no schedule or one that does not parse; a malformed value
// is treated as no schedule, so a typo leaves the ticket open rather than
// running it at an instant nobody asked for.
func (t *Ticket) Schedule() (time.Time, bool) {
	if t.ScheduledAt == "" {
		return time.Time{}, false
	}
	at, err := ParseSchedule(t.ScheduledAt)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// SetSchedule writes the normalized form of at into the frontmatter.
func (t *Ticket) SetSchedule(at time.Time) error {
	return t.SetField(FieldScheduledAt, FormatSchedule(at))
}

// ClearSchedule removes the field. Removing one that is not there is not an
// error, so every lifecycle path can call it unconditionally.
func (t *Ticket) ClearSchedule() error {
	return t.DeleteField(FieldScheduledAt)
}
