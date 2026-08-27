package ticket

import (
	"fmt"
	"regexp"
	"strconv"
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

// scheduleLocalLayouts are the zoneless spellings ParseScheduleFlex accepts
// beside RFC 3339. Both are read in the local zone, which is what a person
// typing one of them at a terminal means.
var scheduleLocalLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"}

// ParseScheduleFlex reads a pickup time the way a person spells one: RFC 3339,
// or a local wall time such as "2026-09-01 09:00".
//
// It is the front door for flag and free-text input only. ParseSchedule stays
// the parser for stored and wire values, because the field is compared as text
// and exactly one spelling may reach it.
//
// A date with no time is refused rather than read as midnight: which midnight
// it means depends on the zone the reader is in, and a schedule that fires a
// day early in another zone is worse than a rejection.
func ParseScheduleFlex(s string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, s); err == nil {
		return at, nil
	}
	for _, layout := range scheduleLocalLayouts {
		// ParseInLocation, not Parse: Parse reads a zoneless string as UTC.
		// A wall time in the fall-back hour is ambiguous and resolves to the
		// first of the two instants; no zoneless spelling can distinguish them.
		if at, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return at, nil
		}
	}
	return time.Time{}, fmt.Errorf("%s must be an RFC 3339 instant such as 2026-09-01T09:00:00Z or a local time such as \"2026-09-01 09:00\", got %q", FieldScheduledAt, s)
}

// scheduleDayUnits matches the day and week units time.ParseDuration does not
// know. A schedule is the one field measured in days, so typing 3d is what a
// person expects to work.
var scheduleDayUnits = regexp.MustCompile(`(\d+(?:\.\d+)?)([dw])`)

// ParseScheduleDelay reads a delay in Go's duration grammar extended with d
// (24h) and w (168h).
//
// The extension is a rewrite to hours rather than its own parser:
// ParseDuration sums repeated units, so "1w2d3h" becomes "168h48h3h" and still
// adds up. Everything Go accepts keeps working, the composite forms included.
//
// Its errors are sentence fragments, so the caller can name the flag or field
// the value came from in front of them.
func ParseScheduleDelay(s string) (time.Duration, error) {
	rewritten := scheduleDayUnits.ReplaceAllStringFunc(s, func(m string) string {
		parts := scheduleDayUnits.FindStringSubmatch(m)
		n, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return m
		}
		hours := 24.0
		if parts[2] == "w" {
			hours = 168.0
		}
		return strconv.FormatFloat(n*hours, 'f', -1, 64) + "h"
	})
	d, err := time.ParseDuration(rewritten)
	if err != nil {
		return 0, fmt.Errorf("takes a duration such as 90m, 24h, 3d or 2w, got %q", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be a positive duration, got %q", s)
	}
	return d, nil
}
