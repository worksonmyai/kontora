package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// LocalClient returns a client for the daemon this config describes. Commands
// that need daemon state — the queue, live runs, a ticket's worktree — go
// through the same HTTP API in local mode as in remote mode, rather than
// keeping a second implementation that reads behind the daemon's back.
// Before returning a client, LocalClient checks over loopback that the daemon
// serves cfg.TicketsDir.
func LocalClient(cfg *config.Config) (*remote.Client, error) {
	const remoteWarning = "Use --url with --token only if you intend to use the daemon's board. Explicit remote mode does not check local tickets_dir."
	if cfg.Web.Enabled != nil && !*cfg.Web.Enabled {
		return nil, fmt.Errorf("local daemon API disabled: web.enabled is false")
	}
	client, err := remote.NewLocal(cfg.Web.Host, cfg.Web.Port, cfg.Web.Token)
	if err != nil {
		address := net.JoinHostPort(web.UnbracketHost(cfg.Web.Host), strconv.Itoa(cfg.Web.Port))
		return nil, fmt.Errorf("web.host %q: %w. %s Example: --url http://%s with --token", cfg.Web.Host, err, remoteWarning, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	identity, err := remote.VerifyBoard(ctx, client, cfg.TicketsDir)
	if err != nil {
		if remote.IsUnauthorized(err) {
			return nil, fmt.Errorf("daemon at %s returned HTTP 401. Set web.token in the local config and retry. %s", client.Base(), remoteWarning)
		}
		if probe, ok := errors.AsType[*remote.BoardProbeError](err); ok {
			return nil, fmt.Errorf("cannot verify daemon board: GET %s/api/config: %w", client.Base(), probe.Cause())
		}
		return nil, fmt.Errorf("%s: %w", client.Base(), err)
	}
	dir, err := config.DirectoryIdentity(identity.TicketsDir)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot resolve verified board %q: %w", client.Base(), identity.TicketsDir, err)
	}
	return client.GuardMutations(func(run func() error) error {
		return ticket.WithOrdinaryRouting(dir, run)
	}), nil
}

// PrintStats renders the throughput figures the Stats page shows.
func PrintStats(info web.StatsInfo, w io.Writer) error {
	fmt.Fprintf(w, "%s  %s → %s (%d days)\n\n",
		styleBold.Render("Window"), info.Window.From, info.Window.To, info.Window.Days)

	fmt.Fprintln(w, styleBold.Render("Totals"))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  shipped\t%d\n", info.Totals.Shipped)
	fmt.Fprintf(tw, "  shipped this week\t%d\n", info.Totals.ShippedThisWeek)
	fmt.Fprintf(tw, "  runs\t%d\n", info.Totals.Runs)
	fmt.Fprintf(tw, "  median cycle\t%s\n", formatMillis(info.Totals.MedianCycleMS))
	fmt.Fprintf(tw, "  first pass\t%.0f%%\n", info.Totals.FirstPassPct)
	fmt.Fprintf(tw, "  tokens\t%d in / %d out\n", info.Totals.TokensIn, info.Totals.TokensOut)
	if info.Totals.BusiestDay != "" {
		fmt.Fprintf(tw, "  busiest day\t%s (%d runs)\n", info.Totals.BusiestDay, info.Totals.BusiestDayRuns)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\n%s  %d running of %d slots, %d queued, %d in review\n",
		styleBold.Render("Live"), info.Live.Running, info.Live.Slots, info.Live.Queued, info.Live.InReview)

	if len(info.Stages) > 0 {
		fmt.Fprintf(w, "\n%s\n", styleBold.Render("Stages"))
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  STAGE\tRUNS\tCOMPLETE\tBLOCKED\tPROTOCOL_ERROR\tFAILED\tUNVERIFIED\tRUNNER_FAILED\tP50\tP90\tTOKENS")
		for _, s := range info.Stages {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%d\n",
				s.Name, s.Runs, s.Complete, s.Blocked, s.ProtocolError, s.Failed, s.Unknown, s.RunnerFailed,
				formatMillis(s.P50MS), formatMillis(s.P90MS), s.Tokens)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(info.Agents) > 0 {
		fmt.Fprintf(w, "\n%s\n", styleBold.Render("Agents"))
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  AGENT\tRUNS\tFIRST PASS\tMEDIAN")
		for _, a := range info.Agents {
			fmt.Fprintf(tw, "  %s\t%d\t%.0f%%\t%s\n", a.Name, a.Runs, a.FirstPassPct, formatMillis(a.MedianMS))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(info.Projects) > 0 {
		fmt.Fprintf(w, "\n%s\n", styleBold.Render("Projects"))
		tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  PROJECT\tDONE\tMEDIAN CYCLE")
		for _, p := range info.Projects {
			fmt.Fprintf(tw, "  %s\t%d\t%s\n", p.Name, p.Done, formatMillis(p.MedianCycleMS))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// PrintChanges renders a ticket's branch: its commits and the per-file line
// counts against the base branch.
func PrintChanges(info web.ChangesInfo, w io.Writer) error {
	if info.Branch == "" {
		fmt.Fprintln(w, "No branch for this ticket yet.")
		return nil
	}
	fmt.Fprintf(w, "%s %s → %s\n", styleBold.Render("Branch"), info.Branch, info.Base)

	if len(info.Commits) == 0 && len(info.Files) == 0 {
		fmt.Fprintln(w, "\nNo changes.")
		return nil
	}

	if len(info.Commits) > 0 {
		fmt.Fprintf(w, "\n%s\n", styleBold.Render("Commits"))
		for _, c := range info.Commits {
			sha := c.SHA
			if len(sha) > 8 {
				sha = sha[:8]
			}
			fmt.Fprintf(w, "  %s  %s\n", styleFaint.Render(sha), c.Subject)
		}
	}

	if len(info.Files) > 0 {
		var added, deleted int
		fmt.Fprintf(w, "\n%s\n", styleBold.Render("Files"))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, f := range info.Files {
			added += f.Added
			deleted += f.Deleted
			fmt.Fprintf(tw, "  %s\t+%d\t-%d\n", f.Path, f.Added, f.Deleted)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		fmt.Fprintf(w, "\n%d file(s), +%d -%d\n", len(info.Files), added, deleted)
	}
	return nil
}

// PrintActivity renders one stage run's transcript. A structured tape is
// printed one event per line; a plaintext fallback is passed through as is.
func PrintActivity(info web.ActivityInfo, w io.Writer) error {
	if info.Content == "" && (info.Tape == nil || len(info.Tape.Events) == 0) {
		fmt.Fprintln(w, "No activity recorded for this ticket yet.")
		return nil
	}

	header := "run"
	if info.Stage != "" {
		header = info.Stage + " run"
	}
	header = fmt.Sprintf("%s %d", header, info.Run)

	var notes []string
	if info.Live {
		notes = append(notes, "live")
	}
	if info.Stale {
		notes = append(notes, "may include another run of this stage")
	}
	if len(notes) > 0 {
		header += " (" + strings.Join(notes, ", ") + ")"
	}
	fmt.Fprintln(w, styleBold.Render(header))

	if info.Tape == nil {
		fmt.Fprint(w, info.Content)
		return nil
	}
	for _, ev := range info.Tape.Events {
		fmt.Fprintf(w, "%s %s\n", styleFaint.Render(ev.Kind), activityLine(ev))
	}
	return nil
}

// activityLine picks the field that carries an event's content: plain events
// hold it in Text, tool events describe themselves with the tool name and its
// argument, and the result line follows underneath.
func activityLine(ev logfmt.Event) string {
	var parts []string
	if ev.Tool != "" {
		parts = append(parts, ev.Tool)
	}
	for _, s := range []string{ev.Arg, ev.Text, ev.Summary} {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

// formatMillis renders a duration held as milliseconds. Zero means the figure
// had nothing to average over, which is not the same as "instant".
func formatMillis(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return FormatDuration(time.Duration(ms) * time.Millisecond)
}
