package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/worksonmyai/kontora/internal/web"
)

// printRemoteTickets renders a static ticket table for remote `ls`. Remote mode
// is non-interactive (no TUI), so output is a plain aligned table.
func printRemoteTickets(w io.Writer, tickets []web.TicketInfo, running int) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tSTAGE\tPIPELINE\tTITLE")
	for _, t := range tickets {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Status, dash(t.Stage), dash(t.Pipeline), dash(t.Title))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d ticket(s), %d running\n", len(tickets), running)
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
