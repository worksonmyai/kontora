package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/web"
)

// printRemoteList renders `kontora ls` in remote mode. It asks for the complete
// ticket set and then runs the same filter, sort and render path as local mode,
// so the flags mean the same thing on both sides.
func printRemoteList(w io.Writer, rc *remote.Client, opts cli.ListOpts) error {
	tickets, _, err := rc.ListAllTickets()
	if err != nil {
		return err
	}
	return cli.RenderList(remoteListing(tickets, opts), w, opts)
}

// remoteListing projects a daemon list payload onto the shared rows and applies
// the filters. Readiness is derived here from the complete set the daemon sent.
func remoteListing(tickets []web.TicketInfo, opts cli.ListOpts) cli.Listing {
	items := make([]cli.ListItem, 0, len(tickets))
	for _, t := range tickets {
		items = append(items, remoteListItem(t))
	}
	cli.ClassifyList(items)
	return cli.FilterList(items, opts)
}

// remoteListItem projects a daemon ticket payload onto the shared row. The
// daemon resolves the project name, because only it has the config.
func remoteListItem(t web.TicketInfo) cli.ListItem {
	return cli.ListItem{
		ID:          t.ID,
		Title:       t.Title,
		Status:      t.Status,
		Kontora:     t.Kontora,
		Path:        t.Path,
		Project:     t.Project,
		Pipeline:    t.Pipeline,
		Stage:       t.Stage,
		Agent:       t.Agent,
		CreatedAt:   t.CreatedAt,
		StartedAt:   t.StartedAt,
		CompletedAt: t.FinishedAt,
		Deps:        refIDs(t.Deps),
		Links:       refIDs(t.Links),
		Parent:      refID(t.Parent),
		ScheduledAt: t.ScheduledAt,
	}
}

func refIDs(refs []web.TicketRef) []string {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	return ids
}

func refID(ref *web.TicketRef) string {
	if ref == nil {
		return ""
	}
	return ref.ID
}

// printRemoteTicket renders a single ticket for remote `view`.
func printRemoteTicket(w io.Writer, t web.TicketInfo) {
	fmt.Fprintf(w, "ID:       %s\n", t.ID)
	fmt.Fprintf(w, "Title:    %s\n", t.Title)
	fmt.Fprintf(w, "Status:   %s\n", t.Status)
	if t.Stage != "" {
		fmt.Fprintf(w, "Stage:    %s\n", t.Stage)
	}
	if t.Pipeline != "" {
		fmt.Fprintf(w, "Pipeline: %s\n", t.Pipeline)
	}
	if t.Agent != "" {
		fmt.Fprintf(w, "Agent:    %s\n", t.Agent)
	}
	if t.Path != "" {
		fmt.Fprintf(w, "Path:     %s\n", t.Path)
	}
	if t.Branch != "" {
		fmt.Fprintf(w, "Branch:   %s\n", t.Branch)
	}
	if t.ScheduledAt != "" {
		fmt.Fprintf(w, "Starts:   %s\n", cli.FormatSchedule(t.ScheduledAt))
	}
	if t.LastError != "" {
		fmt.Fprintf(w, "Error:    %s\n", t.LastError)
	}
	if strings.TrimSpace(t.Body) != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(t.Body, "\n"))
	}
}

// printRemoteConfig renders the daemon's pipelines and agents for remote `config`.
func printRemoteConfig(w io.Writer, cfg web.ConfigInfo) {
	fmt.Fprintf(w, "Default agent: %s\n", dash(cfg.DefaultAgent))
	fmt.Fprintf(w, "Branch prefix: %s\n", dash(cfg.BranchPrefix))

	agents := append([]string(nil), cfg.Agents...)
	sort.Strings(agents)
	fmt.Fprintf(w, "\nAgents: %s\n", strings.Join(agents, ", "))

	fmt.Fprintln(w, "\nPipelines:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range cfg.PipelineInfos {
		fmt.Fprintf(tw, "  %s\t%s\n", p.Name, strings.Join(p.Stages, " -> "))
	}
	_ = tw.Flush()

	if len(cfg.CustomStatuses) > 0 {
		fmt.Fprintf(w, "\nCustom statuses: %s\n", strings.Join(cfg.CustomStatuses, ", "))
	}
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
