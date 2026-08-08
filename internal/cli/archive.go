package cli

import (
	"fmt"
	"io"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// ArchiveOpts holds parameters for the archive command.
type ArchiveOpts struct {
	Days    int
	DryRun  bool
	Path    string
	Project string
	Status  string
}

// Archive marks old done/cancelled tickets as archived and prints a summary to
// w. Option validation lives in the app layer, which rejects a non-positive
// days value, an unusable status, and an unknown project before touching any
// files.
func Archive(cfg *config.Config, w io.Writer, opts ArchiveOpts) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})

	result, err := svc.Archive(app.ArchiveOptions{
		Days:    opts.Days,
		DryRun:  opts.DryRun,
		Path:    opts.Path,
		Project: opts.Project,
		Status:  ticket.Status(opts.Status),
	})

	// Print the IDs that were archived, even when the run failed partway through.
	for _, id := range result.Archived {
		if _, perr := fmt.Fprintln(w, id); perr != nil {
			return perr
		}
	}
	if err != nil {
		return err
	}

	n := len(result.Archived)
	noun := "tickets"
	if n == 1 {
		noun = "ticket"
	}
	if opts.Status != "" {
		noun = opts.Status + " " + noun
	}
	scope := ""
	switch {
	case opts.Project != "":
		scope = fmt.Sprintf(" in project %s", opts.Project)
	case opts.Path != "":
		scope = fmt.Sprintf(" in %s", opts.Path)
	}
	if result.DryRun {
		_, err = fmt.Fprintf(w, "Would archive %d %s closed for at least %d days%s (dry run).\n", n, noun, opts.Days, scope)
	} else {
		_, err = fmt.Fprintf(w, "Archived %d %s closed for at least %d days%s.\n", n, noun, opts.Days, scope)
	}
	return err
}
