package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// printRemoteTickets renders a static ticket table for remote `ls`. Remote mode
// is non-interactive (no TUI), so output is a plain aligned table. Rows are
// filtered and ordered the way local `ls` filters and orders them, so the same
// flags mean the same thing on both sides.
func printRemoteTickets(w io.Writer, tickets []web.TicketInfo, running int, showClosed bool) {
	visible, hasClosed := filterRemoteTickets(tickets, showClosed)

	if len(visible) == 0 {
		if hasClosed {
			fmt.Fprintln(w, "No active tickets. Use --closed to show done/cancelled.")
		} else {
			fmt.Fprintln(w, "No tickets.")
		}
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tSTAGE\tPIPELINE\tTITLE\tMARKER")
	for _, t := range visible {
		marker := ""
		if !t.Kontora {
			marker = "not a kontora ticket"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Status, dash(t.Stage), dash(t.Pipeline), dash(t.Title), marker)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d ticket(s), %d running\n", len(visible), running)
}

// filterRemoteTickets applies the same visibility rules as cli.Status: archived
// tickets never show, and closed ones only under --closed. It reports whether
// any closed ticket was held back so the empty case can say so.
func filterRemoteTickets(tickets []web.TicketInfo, showClosed bool) (visible []web.TicketInfo, hasClosed bool) {
	for _, t := range tickets {
		switch ticket.Status(t.Status) { //nolint:exhaustive // only the hidden statuses matter here
		case ticket.StatusArchived:
			continue
		case ticket.StatusDone, ticket.StatusCancelled:
			hasClosed = true
			if !showClosed {
				continue
			}
		}
		visible = append(visible, t)
	}

	sort.SliceStable(visible, func(i, j int) bool {
		ri := cli.StatusRank(ticket.Status(visible[i].Status))
		rj := cli.StatusRank(ticket.Status(visible[j].Status))
		if ri != rj {
			return ri < rj
		}
		return visible[i].ID < visible[j].ID
	})
	return visible, hasClosed
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
