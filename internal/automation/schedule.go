package automation

import (
	"fmt"
	"strings"
	"time"
)

func (s Schedule) Validate() error {
	switch s.Frequency {
	case "daily", "weekdays":
		if len(s.Days) != 0 {
			return validation("days are only valid for weekly schedules")
		}
	case "weekly":
		if len(s.Days) == 0 || len(s.Days) > 7 {
			return validation("weekly schedules need days (Sunday=0)")
		}
		var seen [7]bool
		for _, d := range s.Days {
			if d < 0 || d > 6 || seen[d] {
				return validation("invalid or duplicate weekday")
			}
			seen[d] = true
		}
	default:
		return validation("unsupported frequency %q", s.Frequency)
	}
	if len(s.Time) != 5 || s.Time[2] != ':' {
		return validation("time must be HH:MM")
	}
	if _, err := time.Parse("15:04", s.Time); err != nil {
		return validation("time must be HH:MM")
	}
	if s.Timezone == "" || s.Timezone == "Local" || s.Timezone == "localtime" || strings.ContainsAny(s.Timezone, "\\\x00") || strings.HasPrefix(s.Timezone, "/") || strings.Contains(s.Timezone, "..") || len(s.Timezone) > 200 {
		return validation("timezone must be an explicit IANA name")
	}
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return validation("invalid IANA timezone: %v", err)
	}
	return nil
}

func (s Schedule) selected(day time.Weekday) bool {
	switch s.Frequency {
	case "daily":
		return true
	case "weekdays":
		return day != time.Saturday && day != time.Sunday
	default:
		for _, d := range s.Days {
			if int(day) == d {
				return true
			}
		}
		return false
	}
}

func (s Schedule) Next(after time.Time) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	if after.Year() < 1 || after.Year() > 9999 {
		return time.Time{}, validation("after is out of range")
	}
	end := after.AddDate(0, 0, 16)
	ceiling := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	if end.After(ceiling) {
		end = ceiling
	}
	first, _, _, err := s.between(after.Add(time.Nanosecond), end)
	if err != nil {
		return time.Time{}, err
	}
	if first.IsZero() {
		return time.Time{}, validation("no next occurrence in supported date range")
	}
	return first, nil
}

func (s Schedule) Preview(after time.Time, n int) ([]time.Time, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if n < 0 || n > 1000 {
		return nil, validation("preview count must be between 0 and 1000")
	}
	result := make([]time.Time, 0, n)
	for range n {
		next, err := s.Next(after)
		if err != nil {
			return nil, err
		}
		result = append(result, next)
		after = next
	}
	return result, nil
}

func (s Schedule) LatestDue(first, now time.Time) (time.Time, int64, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, 0, err
	}
	if !validInstant(first) || !validInstant(now) {
		return time.Time{}, 0, validation("due range is out of bounds")
	}
	_, latest, count, err := s.between(first, now)
	return latest, count, err
}

func (s Schedule) between(first, now time.Time) (earliest, latest time.Time, count int64, err error) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return earliest, latest, 0, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	clock, _ := time.Parse("15:04", s.Time)
	for cursor := first.UTC(); !cursor.After(now); {
		local := cursor.In(loc)
		_, offset := local.Zone()
		start, end := local.ZoneBounds()
		if !start.IsZero() {
			start = start.UTC()
		}
		if !end.IsZero() {
			end = end.UTC()
		}
		lo, hi := cursor, now.UTC()
		if !end.IsZero() && !end.After(cursor) {
			end = time.Date(cursor.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if !end.IsZero() && !end.After(hi) {
			hi = end.Add(-time.Nanosecond)
		}
		if !start.IsZero() {
			_, previous := start.Add(-time.Nanosecond).In(loc).Zone()
			if previous > offset {
				foldEnd := start.Add(time.Duration(previous-offset) * time.Second)
				if lo.Before(foldEnd) {
					lo = foldEnd
				}
			}
		}
		if !lo.After(hi) {
			wallLo := lo.Add(time.Duration(offset) * time.Second)
			wallHi := hi.Add(time.Duration(offset) * time.Second)
			a := time.Date(wallLo.Year(), wallLo.Month(), wallLo.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
			b := time.Date(wallHi.Year(), wallHi.Month(), wallHi.Day(), clock.Hour(), clock.Minute(), 0, 0, time.UTC)
			if a.Before(wallLo) {
				a = a.AddDate(0, 0, 1)
			}
			if b.After(wallHi) {
				b = b.AddDate(0, 0, -1)
			}
			if !a.After(b) {
				days := (b.Unix()-a.Unix())/86400 + 1
				perWeek := int64(0)
				for d := range 7 {
					if s.selected(time.Weekday(d)) {
						perWeek++
					}
				}
				n := days / 7 * perWeek
				for d := int64(0); d < days%7; d++ {
					if s.selected(time.Weekday((int64(a.Weekday()) + d) % 7)) {
						n++
					}
				}
				if n > 0 {
					for !s.selected(a.Weekday()) {
						a = a.AddDate(0, 0, 1)
					}
					for !s.selected(b.Weekday()) {
						b = b.AddDate(0, 0, -1)
					}
					if earliest.IsZero() {
						earliest = a.Add(-time.Duration(offset) * time.Second)
					}
					latest = b.Add(-time.Duration(offset) * time.Second)
					count += n
				}
			}
		}
		if end.IsZero() || end.After(now) {
			break
		}
		cursor = end.UTC()
	}
	return earliest, latest, count, nil
}
