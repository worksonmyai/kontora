package automation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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

type Spec struct {
	Name      string `json:"name" yaml:"name"`
	Prompt    string `json:"prompt" yaml:"prompt"`
	Path      string `json:"path" yaml:"path"`
	Agent     string `json:"agent" yaml:"agent"`
	Model     string `json:"model" yaml:"model"`
	Schedule  string `json:"schedule" yaml:"schedule"`
	Timezone  string `json:"timezone" yaml:"timezone"`
	Workspace string `json:"workspace" yaml:"workspace"`
	Overlap   string `json:"overlap" yaml:"overlap"`
	Timeout   string `json:"timeout" yaml:"timeout"`
	Enabled   bool   `json:"enabled" yaml:"enabled"`
}

func (s Spec) Normalize() Spec {
	if s.Timezone == "" {
		s.Timezone = "UTC"
	}
	if s.Workspace == "" {
		s.Workspace = "worktree"
	}
	if s.Overlap == "" {
		s.Overlap = "skip"
	}
	if s.Timeout == "" {
		s.Timeout = "30m"
	}
	return s
}

func (s Spec) Validate() error {
	s = s.Normalize()
	for _, f := range []struct {
		name, value         string
		limit               int
		multiline, required bool
	}{
		{"name", s.Name, 200, false, true}, {"prompt", s.Prompt, 65536, true, true},
		{"path", s.Path, 4096, false, true}, {"agent", s.Agent, 200, false, true},
		{"model", s.Model, 200, false, false},
	} {
		if !textValid(f.value, f.limit, f.multiline) || (f.required && strings.TrimSpace(f.value) == "") {
			return validation("invalid %s", f.name)
		}
	}
	if s.Workspace != "worktree" && s.Workspace != "repo" {
		return validation("workspace must be worktree or repo")
	}
	if s.Overlap != "skip" && s.Overlap != "queue" && s.Overlap != "replace" {
		return validation("overlap must be skip, queue or replace")
	}
	if d, err := time.ParseDuration(s.Timeout); err != nil || d <= 0 {
		return validation("timeout must be a positive duration")
	}
	_, err := Compile(s.Schedule, s.Timezone)
	return err
}

type Definition struct {
	Spec
	ID            string      `json:"id"`
	Key           string      `json:"key"`
	Revision      uint64      `json:"revision"`
	CreatedAt     time.Time   `json:"created_at"`
	Next          time.Time   `json:"next"`
	NextRuns      []time.Time `json:"next_runs"`
	ScheduleHuman string      `json:"schedule_human"`
	Project       string      `json:"project"`
	Running       bool        `json:"running"`
	CurrentRun    *Run        `json:"current_run"`
	LastRun       *Run        `json:"last_run"`
	RunsTotal     int         `json:"runs_total"`
	SuccessRate   *float64    `json:"success_rate"`
	AvgWall       *float64    `json:"avg_wall"`
	Spend30d      *string     `json:"spend_30d"`
	PricedRuns    int         `json:"priced_runs"`
	TrackedRuns   int         `json:"tracked_runs"`
}

type Admission struct {
	SkipReason string
	Supersede  []uint64
	StopQueued []uint64
}

type Run struct {
	AutomationID     string    `json:"automation_id"`
	Key              string    `json:"key"`
	N                uint64    `json:"n"`
	Revision         uint64    `json:"revision"`
	InvocationID     string    `json:"invocation_id"`
	Phase            string    `json:"phase"`
	Outcome          string    `json:"outcome"`
	Trigger          string    `json:"trigger"`
	ScheduledFor     time.Time `json:"scheduled_for"`
	AcceptedAt       time.Time `json:"accepted_at"`
	StartedAt        time.Time `json:"started_at"`
	CompletedAt      time.Time `json:"completed_at"`
	ExitCode         *int      `json:"exit_code"`
	ObservedExitCode *int      `json:"observed_exit_code"`
	Result           string    `json:"result"`
	Cost             *string   `json:"cost"`
	Spec             Spec      `json:"spec"`
	Session          string    `json:"session"`
	Window           string    `json:"window"`
	WindowID         string    `json:"window_id"`
	SessionID        string    `json:"session_id"`
	SessionDir       string    `json:"session_dir"`
	Worktree         string    `json:"worktree"`
	Branch           string    `json:"branch"`
	BaseCommit       string    `json:"base_commit"`
	LogPath          string    `json:"log_path"`
	ResultPath       string    `json:"result_path"`
	StopReason       string    `json:"stop_reason"`
	Termination      string    `json:"termination"`
}

type LogLine struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
}

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

func ValidateKey(key string) error {
	if len(key) > 100 || !keyPattern.MatchString(key) {
		return validation("key must be a lowercase kebab-case name (max 100 bytes)")
	}
	return nil
}

func validation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}
func textValid(s string, limit int, multiline bool) bool {
	return utf8.ValidString(s) && len(s) <= limit && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && (!multiline || (r != '\n' && r != '\r' && r != '\t'))
	})
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func IsID(id string) bool { return validID(id) }

func validID(id string) bool {
	if !strings.HasPrefix(id, "aut-") {
		return false
	}
	b, err := hex.DecodeString(id[4:])
	return err == nil && len(b) == 16 && strings.ToLower(id) == id
}
func validInstant(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func cloneRun(r Run) Run {
	if r.ExitCode != nil {
		r.ExitCode = new(*r.ExitCode)
	}
	if r.ObservedExitCode != nil {
		r.ObservedExitCode = new(*r.ObservedExitCode)
	}
	if r.Cost != nil {
		r.Cost = new(*r.Cost)
	}
	return r
}
