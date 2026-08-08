package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

// archiveTitleWidth caps the title column so one long heading cannot push the
// path column off the screen.
const archiveTitleWidth = 60

// ArchiveOpts holds parameters for the archive command.
type ArchiveOpts struct {
	Days    int
	DryRun  bool
	Path    string
	Project string
	Status  string
	// Yes archives without asking for confirmation.
	Yes bool
	// In is where the answer to the confirmation prompt is read from. A nil
	// reader means no terminal is attached: a run that would write files then
	// fails and asks for --yes instead of prompting into the void.
	In io.Reader
}

// Archive marks old done/cancelled tickets as archived and prints a summary to
// w. It first lists the matching tickets, then asks for confirmation unless
// opts.Yes is set. Option validation lives in the app layer, which rejects a
// non-positive days value, an unusable status, and an unknown project before
// touching any files.
func Archive(cfg *config.Config, w io.Writer, opts ArchiveOpts) error {
	repo := store.NewDiskRepo(cfg.TicketsDir)
	svc := app.New(app.Static(cfg), repo, app.NoopRuntime{})
	appOpts := app.ArchiveOptions{
		Days:    opts.Days,
		DryRun:  true,
		Path:    opts.Path,
		Project: opts.Project,
		Status:  ticket.Status(opts.Status),
	}

	// Preview first: the same selection as the real run, with no writes. It
	// feeds both the listing and the confirmation prompt.
	preview, err := svc.Archive(appOpts)
	if err != nil {
		return err
	}

	if len(preview.Archived) == 0 {
		_, err := fmt.Fprintln(w, styleFaint.Render(fmt.Sprintf("No %s closed for at least %s%s.",
			archiveNoun(opts, 0), archiveDays(opts.Days), archiveScope(opts))))
		return err
	}

	fmt.Fprintln(w, styleBold.Render(fmt.Sprintf("%d %s", len(preview.Archived), archiveNoun(opts, len(preview.Archived))))+
		styleFaint.Render(fmt.Sprintf(" closed for at least %s%s", archiveDays(opts.Days), archiveScope(opts))))
	fmt.Fprintln(w, renderArchiveTable(preview.Archived))

	if opts.DryRun {
		_, err := fmt.Fprintln(w, styleFaint.Render("Dry run: no files changed."))
		return err
	}

	if !opts.Yes {
		ok, err := confirmArchive(w, opts.In, len(preview.Archived), archiveNoun(opts, len(preview.Archived)))
		if err != nil {
			return err
		}
		if !ok {
			_, err := fmt.Fprintln(w, styleWarn.Render("Cancelled: no files changed."))
			return err
		}
	}

	appOpts.DryRun = false
	result, err := svc.Archive(appOpts)

	// Report the tickets that were archived, even when the run failed partway
	// through.
	n := len(result.Archived)
	if n > 0 {
		fmt.Fprintf(w, "%s Archived %d %s.\n", styleOK.Render("✓"), n, archiveNoun(opts, n))
	}
	if err != nil {
		return err
	}
	if n == 0 {
		_, err = fmt.Fprintln(w, styleFaint.Render("Nothing was archived."))
	}
	return err
}

// confirmArchive asks whether to archive n tickets and reads the answer from
// in. Anything but y or yes, including an empty line or a closed stdin, means
// no.
func confirmArchive(w io.Writer, in io.Reader, n int, noun string) (bool, error) {
	if in == nil {
		return false, fmt.Errorf("refusing to archive %d %s without a terminal to confirm on: pass --yes", n, noun)
	}
	fmt.Fprintf(w, "%s %s ", styleBold.Render(fmt.Sprintf("Archive %d %s?", n, noun)), styleFaint.Render("[y/N]"))

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	// The prompt has no trailing newline, so close the line the user typed on
	// when the answer came from a pipe rather than a terminal echo.
	fmt.Fprintln(w)

	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func renderArchiveTable(entries []app.ArchiveEntry) string {
	pad := lipgloss.NewStyle().PaddingRight(3)
	statuses := make([]ticket.Status, len(entries))

	tbl := table.New().
		Headers("ID", "STATUS", "TITLE", "PATH").
		Border(lipgloss.HiddenBorder()).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return pad.Bold(true).Faint(true)
			}
			switch col {
			case 0: // ID
				return pad.Foreground(lipgloss.Color("6")) // cyan
			case 1: // STATUS
				if row >= 0 && row < len(statuses) && statuses[row] == ticket.StatusCancelled {
					return pad.Foreground(lipgloss.Color("3")) // yellow
				}
				return pad.Foreground(lipgloss.Color("2")) // green
			case 3: // PATH
				return pad.Faint(true)
			}
			return pad
		})

	for i, e := range entries {
		statuses[i] = e.Status
		tbl.Row(e.ID, string(e.Status), dashIfEmpty(truncate(e.Title, archiveTitleWidth)), dashIfEmpty(e.Path))
	}
	return tbl.Render()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// archiveNoun names what the run acts on, singular for one ticket and narrowed
// by the status filter when there is one: "3 cancelled tickets".
func archiveNoun(opts ArchiveOpts, n int) string {
	noun := "tickets"
	if n == 1 {
		noun = "ticket"
	}
	if opts.Status != "" {
		noun = opts.Status + " " + noun
	}
	return noun
}

func archiveDays(days int) string {
	if days == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", days)
}

// archiveScope describes the repository filter for the summary lines, empty
// when the run covers every ticket.
func archiveScope(opts ArchiveOpts) string {
	switch {
	case opts.Project != "":
		return fmt.Sprintf(" in project %s", opts.Project)
	case opts.Path != "":
		return fmt.Sprintf(" in %s", opts.Path)
	}
	return ""
}
