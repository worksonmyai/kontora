package automation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrValidation  = errors.New("invalid automation")
	ErrConflict    = errors.New("automation revision or request conflict")
	ErrNotFound    = errors.New("automation not found")
	ErrUnavailable = errors.New("automation storage unavailable")
)

type Schedule struct {
	Frequency string `json:"frequency"`
	Days      []int  `json:"days"`
	Time      string `json:"time"`
	Timezone  string `json:"timezone"`
}

type Definition struct {
	ID               string    `json:"id"`
	Revision         uint64    `json:"revision"`
	ScheduleRevision uint64    `json:"schedule_revision"`
	Name             string    `json:"name"`
	Path             string    `json:"path"`
	Agent            string    `json:"agent"`
	Instructions     string    `json:"instructions"`
	Schedule         Schedule  `json:"schedule"`
	Enabled          bool      `json:"enabled"`
	SkipOverlap      bool      `json:"skip_overlap"`
	Next             time.Time `json:"next"`
}

type Occurrence struct {
	ID               string     `json:"id"`
	Revision         uint64     `json:"revision"`
	AutomationID     string     `json:"automation_id"`
	ScheduleRevision uint64     `json:"schedule_revision"`
	ScheduledFor     time.Time  `json:"scheduled_for"`
	Through          time.Time  `json:"through"`
	Count            int64      `json:"count"`
	TicketID         string     `json:"ticket_id"`
	Phase            string     `json:"phase"`
	Reason           string     `json:"reason"`
	Payload          Definition `json:"payload"`
}

func validation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

func textValid(s string, limit int, multiline bool) bool {
	return utf8.ValidString(s) && len(s) <= limit && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t' || r == '\r'))
	})
}

func validateDefinition(d Definition) error {
	for _, f := range []struct {
		name, value string
		limit       int
		multiline   bool
	}{
		{"name", d.Name, 200, false},
		{"path", d.Path, 4096, false},
		{"agent", d.Agent, 200, false},
		{"instructions", d.Instructions, 65536, true},
	} {
		if len(f.value) > f.limit {
			return validation("%s must be at most %d bytes", f.name, f.limit)
		}
		if strings.TrimSpace(f.value) == "" || !textValid(f.value, f.limit, f.multiline) {
			return validation("invalid %s", f.name)
		}
	}
	return d.Schedule.Validate()
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && strings.ToLower(id) == id
}

func cloneDefinition(d Definition) Definition {
	d.Schedule.Days = append([]int{}, d.Schedule.Days...)
	return d
}

func validInstant(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
