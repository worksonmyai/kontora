package app

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// archivableStatuses are the closed statuses the sweep accepts as input.
var archivableStatuses = map[ticket.Status]bool{
	ticket.StatusDone:         true,
	ticket.StatusCancelled:    true,
	ticket.StatusLegacyClosed: true,
}

// ArchiveOptions controls the archive use-case.
type ArchiveOptions struct {
	// Days is the age threshold. A ticket is eligible only when its file mtime
	// is at or before now-Days. Must be >= 1.
	Days int
	// DryRun reports the eligible tickets without writing any files.
	DryRun bool
	// Path, when set, limits the run to tickets whose repository path is this
	// one. Both sides are compared after tilde expansion and cleaning, and only
	// the complete path matches, as in config.ProjectFor.
	Path string
	// Project names a configured project whose path to filter on. It selects
	// the same tickets as Path and may not be combined with it.
	Project string
	// Status, when set, narrows the run to one closed status: done, cancelled,
	// or the legacy closed. Empty means all three.
	Status ticket.Status
}

// ArchiveEntry describes one ticket an archive run selected.
type ArchiveEntry struct {
	ID string
	// Title is the first markdown heading of the body, empty when there is none.
	Title string
	// Path is the ticket's repository path, empty when the ticket has no path
	// field.
	Path string
	// Status is the closed status the ticket had before the run: done,
	// cancelled, or legacy closed.
	Status ticket.Status
}

// ArchiveResult summarizes an archive run.
type ArchiveResult struct {
	// Archived holds the tickets that were archived, or that would be archived
	// in a dry run, sorted by ID.
	Archived []ArchiveEntry
	// DryRun mirrors the option, so callers can format their summary.
	DryRun bool
}

// IDs returns the archived ticket IDs, in the order of Archived.
func (r ArchiveResult) IDs() []string {
	if len(r.Archived) == 0 {
		return nil
	}
	ids := make([]string, len(r.Archived))
	for i, e := range r.Archived {
		ids[i] = e.ID
	}
	return ids
}

// Archive marks old closed tickets as archived. A ticket is eligible only when
// its status is done, cancelled, or the legacy closed that tickets from the
// external ticket CLI carry, and its markdown file mtime is at or before the
// cutoff computed from opts.Days, and when it passes the optional repository and
// status filters. The sweep only ever writes archived; nothing writes closed.
func (s *Service) Archive(opts ArchiveOptions) (ArchiveResult, error) {
	return s.archive(opts, time.Now(), os.Stat)
}

// archivePath resolves the repository path an archive run filters on. An
// unknown project name is an error rather than a run that matches nothing,
// since a typo would otherwise look like "no tickets are old enough".
func (s *Service) archivePath(opts ArchiveOptions) (string, error) {
	path, err := s.cfg().ResolveFilterPath(opts.Path, opts.Project)
	if err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	return path, nil
}

// archive is the testable core of Archive. now sets the reference point for the
// cutoff and stat resolves file mtime; production code passes time.Now and
// os.Stat.
func (s *Service) archive(opts ArchiveOptions, now time.Time, stat func(string) (os.FileInfo, error)) (ArchiveResult, error) {
	if opts.Days < 1 {
		return ArchiveResult{}, fmt.Errorf("archive: days must be a positive number, got %d", opts.Days)
	}
	if opts.Status != "" && !archivableStatuses[opts.Status] {
		return ArchiveResult{}, fmt.Errorf("archive: status must be %s, %s or %s, got %q",
			ticket.StatusDone, ticket.StatusCancelled, ticket.StatusLegacyClosed, opts.Status)
	}
	filterPath, err := s.archivePath(opts)
	if err != nil {
		return ArchiveResult{}, err
	}

	cutoff := now.AddDate(0, 0, -opts.Days)
	wantPath := config.NormalizeRepoPath(filterPath)

	stored, err := s.repo.List()
	if err != nil {
		return ArchiveResult{}, err
	}

	result := ArchiveResult{DryRun: opts.DryRun}
	for _, st := range stored {
		t := st.Ticket
		if t.ID == "" {
			continue
		}
		if !archivableStatuses[t.Status] {
			continue
		}
		if opts.Status != "" && t.Status != opts.Status {
			continue
		}
		if wantPath != "" && config.NormalizeRepoPath(t.Path) != wantPath {
			continue
		}

		info, err := stat(st.FilePath)
		if err != nil {
			return result, fmt.Errorf("stat %s: %w", st.FilePath, err)
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		// Capture the entry before the write, so Status holds the closed status
		// the ticket had rather than archived.
		entry := ArchiveEntry{ID: t.ID, Title: t.Title(), Path: t.Path, Status: t.Status}

		if opts.DryRun {
			result.Archived = append(result.Archived, entry)
			continue
		}

		if err := setArchiveFields(t, t.Status, now, ArchivedBySweep, ""); err != nil {
			return result, fmt.Errorf("archiving %s: %w", t.ID, err)
		}
		if err := s.repo.Save(st); err != nil {
			return result, fmt.Errorf("saving %s: %w", t.ID, err)
		}
		// Record the entry only after the file is persisted, so a mid-loop
		// failure reports the tickets that were actually archived.
		result.Archived = append(result.Archived, entry)
		s.runtime.BroadcastUpdated(t.ID)
	}

	slices.SortFunc(result.Archived, func(a, b ArchiveEntry) int {
		return strings.Compare(a.ID, b.ID)
	})
	return result, nil
}

// The values archived_by carries, naming which surface archived the ticket.
const (
	ArchivedBySweep = "sweep"
	ArchivedByWeb   = "web"
)

// setArchiveFields writes status: archived plus the four-field archive stamp.
// from is the closed status the ticket is leaving, which a restore writes back.
func setArchiveFields(t *ticket.Ticket, from ticket.Status, at time.Time, by, note string) error {
	fields := []struct {
		key   string
		value any
	}{
		{"status", string(ticket.StatusArchived)},
		{"archived_from", string(from)},
		{"archived_at", at.UTC()},
		{"archived_by", by},
	}
	for _, f := range fields {
		if err := t.SetField(f.key, f.value); err != nil {
			return fmt.Errorf("setting %s: %w", f.key, err)
		}
	}
	// An absent note is no note, not an empty one: the rail renders the section
	// only when the field is there.
	if note == "" {
		return t.DeleteField("archive_note")
	}
	return t.SetField("archive_note", note)
}

// ArchiveTicket archives one closed ticket on purpose, recording the status it
// held, when, who asked (by) and an optional note. Unlike the sweep it ignores
// the ticket's age. A ticket in any other status is refused rather than
// archived, so the board cannot lose a ticket that is still being worked on.
func (s *Service) ArchiveTicket(id, note, by string) (Result, error) {
	return s.archiveTicket(id, note, by, time.Now())
}

func (s *Service) archiveTicket(id, note, by string, now time.Time) (Result, error) {
	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	t := st.Ticket
	if !archivableStatuses[t.Status] {
		return Result{}, fmt.Errorf("%w: cannot archive ticket %s in status %s (only %s, %s or %s can be archived)",
			ErrInvalidState, resolved, t.Status, ticket.StatusDone, ticket.StatusCancelled, ticket.StatusLegacyClosed)
	}

	if err := setArchiveFields(t, t.Status, now, by, note); err != nil {
		return Result{}, err
	}
	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: string(ticket.StatusArchived)}, nil
}

// RestoreTicket returns an archived ticket to the status it held before, and
// removes the archive stamp. A ticket archived before the stamp existed, or one
// whose archived_from names a status the config no longer maps to a board
// column, restores to done: stranding it in archived with no way out would be
// worse than putting it in the wrong column.
func (s *Service) RestoreTicket(id string) (Result, error) {
	resolved, err := s.repo.Resolve(id)
	if err != nil {
		return Result{}, err
	}
	st, err := s.repo.Get(resolved)
	if err != nil {
		return Result{}, err
	}

	t := st.Ticket
	if t.Status != ticket.StatusArchived {
		return Result{}, fmt.Errorf("%w: ticket %s is not archived (status %s)", ErrInvalidState, resolved, t.Status)
	}

	target := t.ArchivedFrom
	if !s.cfg().IsBoardStatus(string(target)) {
		target = ticket.StatusDone
	}

	if err := t.SetField("status", string(target)); err != nil {
		return Result{}, fmt.Errorf("setting status: %w", err)
	}
	for _, key := range []string{"archived_from", "archived_at", "archived_by", "archive_note"} {
		if err := t.DeleteField(key); err != nil {
			return Result{}, fmt.Errorf("clearing %s: %w", key, err)
		}
	}
	if err := s.repo.Save(st); err != nil {
		return Result{}, err
	}

	s.runtime.ReconcileDependencies(resolved)
	s.runtime.BroadcastUpdated(resolved)
	return Result{ID: resolved, Status: string(target)}, nil
}
