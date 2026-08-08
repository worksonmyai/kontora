package app

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

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
	// Status, when set, narrows the run to one closed status: done or
	// cancelled. Empty means both.
	Status ticket.Status
}

// ArchiveResult summarizes an archive run.
type ArchiveResult struct {
	// Archived holds the IDs of the tickets that were archived, or that would
	// be archived in a dry run, sorted by ID.
	Archived []string
	// DryRun mirrors the option, so callers can format their summary.
	DryRun bool
}

// Archive marks old done/cancelled tickets as archived. A ticket is eligible
// only when its status is done or cancelled and its markdown file mtime is at
// or before the cutoff computed from opts.Days, and when it passes the optional
// repository and status filters.
func (s *Service) Archive(opts ArchiveOptions) (ArchiveResult, error) {
	return s.archive(opts, time.Now(), os.Stat)
}

// archivePath resolves the repository path an archive run filters on: the one
// given directly, or the path of the named project. An unknown project name is
// an error rather than a run that matches nothing, since a typo would otherwise
// look like "no tickets are old enough".
func (s *Service) archivePath(opts ArchiveOptions) (string, error) {
	if opts.Project == "" {
		return opts.Path, nil
	}
	if opts.Path != "" {
		return "", fmt.Errorf("archive: path and project cannot be combined")
	}
	project, ok := s.cfg().Projects[opts.Project]
	if !ok {
		return "", fmt.Errorf("archive: unknown project %q", opts.Project)
	}
	return project.Path, nil
}

// archive is the testable core of Archive. now sets the reference point for the
// cutoff and stat resolves file mtime; production code passes time.Now and
// os.Stat.
func (s *Service) archive(opts ArchiveOptions, now time.Time, stat func(string) (os.FileInfo, error)) (ArchiveResult, error) {
	if opts.Days < 1 {
		return ArchiveResult{}, fmt.Errorf("archive: days must be a positive number, got %d", opts.Days)
	}
	if opts.Status != "" && opts.Status != ticket.StatusDone && opts.Status != ticket.StatusCancelled {
		return ArchiveResult{}, fmt.Errorf("archive: status must be %s or %s, got %q", ticket.StatusDone, ticket.StatusCancelled, opts.Status)
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
		if t.Status != ticket.StatusDone && t.Status != ticket.StatusCancelled {
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

		if opts.DryRun {
			result.Archived = append(result.Archived, t.ID)
			continue
		}

		if err := t.SetField("status", string(ticket.StatusArchived)); err != nil {
			return result, fmt.Errorf("setting status for %s: %w", t.ID, err)
		}
		if err := s.repo.Save(st); err != nil {
			return result, fmt.Errorf("saving %s: %w", t.ID, err)
		}
		// Record the ID only after the file is persisted, so a mid-loop failure
		// reports the tickets that were actually archived.
		result.Archived = append(result.Archived, t.ID)
		s.runtime.BroadcastUpdated(t.ID)
	}

	slices.Sort(result.Archived)
	return result, nil
}
