package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

type Schedule struct {
	cron     cron.Schedule
	interval time.Duration
	location *time.Location
}

func Compile(schedule, timezone string) (*Schedule, error) {
	if timezone == "" {
		timezone = "UTC"
	}
	if timezone == "Local" || timezone == "localtime" || len(timezone) > 200 || strings.HasPrefix(timezone, "/") || strings.Contains(timezone, "..") || strings.ContainsAny(timezone, "\\\x00") {
		return nil, validation("timezone must be an explicit IANA name")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, validation("invalid IANA timezone: %v", err)
	}
	if strings.TrimSpace(schedule) == "" {
		return nil, validation("a schedule is required")
	}
	fields := strings.Fields(schedule)
	if len(fields) == 1 && !strings.ContainsAny(schedule, "* /,-") {
		if d, err := time.ParseDuration(schedule); err == nil {
			if d < time.Minute || d%time.Minute != 0 {
				return nil, validation("interval must be a positive whole number of minutes")
			}
			return &Schedule{interval: d, location: loc}, nil
		}
	}
	if len(fields) != 5 {
		return nil, validation("cron takes 5 fields (minute hour day month weekday), got %d", len(fields))
	}
	for _, field := range fields {
		for _, r := range field {
			if (r < '0' || r > '9') && !strings.ContainsRune("*/-,", r) {
				return nil, validation("cron fields take digits, *, /, - and , only")
			}
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	parsed, err := parser.Parse(strings.Join(fields, " "))
	if err != nil {
		return nil, validation("invalid cron: %v", err)
	}
	// Starting in a leap year admits February 29 but rejects calendars with no possible date.
	if parsed.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, loc)).IsZero() {
		return nil, validation("cron has no possible occurrence")
	}
	return &Schedule{cron: parsed, location: loc}, nil
}

func (s *Schedule) Next(after time.Time) time.Time {
	if s.interval != 0 {
		return after.Add(s.interval).In(s.location)
	}
	return s.cron.Next(after.In(s.location))
}

func Preview(schedule, timezone string, after time.Time, count int) ([]time.Time, error) {
	if count < 0 || count > 1000 {
		return nil, validation("preview count must be between 0 and 1000")
	}
	if !validInstant(after) {
		return nil, validation("after is out of range")
	}
	s, err := Compile(schedule, timezone)
	if err != nil {
		return nil, err
	}
	out := make([]time.Time, 0, count)
	for range count {
		after = s.Next(after)
		if !validInstant(after) {
			return nil, validation("no next occurrence in supported date range")
		}
		out = append(out, after)
	}
	return out, nil
}

func Human(schedule string) string {
	if d, err := time.ParseDuration(schedule); err == nil && d >= time.Minute && d%time.Minute == 0 {
		if d == time.Hour {
			return "every hour"
		}
		if d%time.Hour == 0 {
			return fmt.Sprintf("every %dh", d/time.Hour)
		}
		return fmt.Sprintf("every %dm", d/time.Minute)
	}
	f := strings.Fields(schedule)
	if len(f) != 5 {
		return schedule
	}
	if f[0] == "0" && f[1] == "*" && strings.Join(f[2:], " ") == "* * *" {
		return "every hour, on the hour"
	}
	if f[0] == "0" && strings.HasPrefix(f[1], "*/") && strings.Join(f[2:], " ") == "* * *" {
		return "every " + strings.TrimPrefix(f[1], "*/") + "h"
	}
	m, me := strconv.Atoi(f[0])
	h, he := strconv.Atoi(f[1])
	if me == nil && he == nil && f[2] == "*" && f[3] == "*" {
		clock := fmt.Sprintf("%02d:%02d", h, m)
		if f[4] == "*" {
			return "daily at " + clock
		}
		if f[4] == "1-5" {
			return "weekdays at " + clock
		}
		if d, e := strconv.Atoi(f[4]); e == nil && d >= 0 && d <= 6 {
			return time.Weekday(d).String() + " at " + clock
		}
	}
	return strings.Join(f, " ")
}
