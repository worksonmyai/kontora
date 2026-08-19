package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// SessionsOptions selects what `kontora sessions` prints.
type SessionsOptions struct {
	// Stage, when set, keeps only that stage and the annotation runs that
	// borrowed its name.
	Stage string
	// Run, when set, keeps only that run number. It drops the stage log and the
	// files no run claims, neither of which has a run number. It is a pointer
	// because 0 is a real run number, so a zero value cannot mean "every run".
	Run *int
	// Logs, Events and Sessions select artifact kinds and are additive. None
	// selected means sessions, which is the reason to run the command.
	Logs   bool
	Events bool
	All    bool
}

// Sessions prints the file paths behind a ticket's runs: one row per history
// row, in history order, then the files no row claims.
//
// A run whose session cannot be found is printed with the reason in place of
// the path. Dropping it would read as "this stage never ran", which is the one
// thing the output must never say by accident.
func Sessions(cfg *config.Config, taskID string, opts SessionsOptions, w io.Writer) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolvedID, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}
	t, err := ticket.ParseFile(filepath.Join(tasksDir, resolvedID+".md"))
	if err != nil {
		return fmt.Errorf("reading ticket: %w", err)
	}

	layout := session.Layout{
		LogsDir:         config.ExpandTilde(cfg.LogsDir),
		TicketID:        resolvedID,
		ClaudeConfigDir: config.ExpandTilde(claudeConfigDir(cfg)),
	}
	// History can be empty while files exist: a pipeline-less ticket appends no
	// history row at all, and a stage appends its row only once its agent exits.
	// The sweep for unclaimed files is the only thing that reports either.
	files := session.Runs(layout, artifacts(opts), historyRuns(cfg, t))
	if len(files) == 0 {
		fmt.Fprintf(w, "%s: no runs recorded\n", resolvedID)
		return nil
	}

	printed := 0
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, f := range files {
		if !keep(f, opts) {
			continue
		}
		printed++
		path := f.Path
		if path == "" {
			path = "-"
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s", f.Stage, runColumn(f), f.Artifact, path)
		if f.Note != "" {
			line += "\t" + f.Note
		}
		fmt.Fprintln(tw, line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if printed == 0 {
		fmt.Fprintf(w, "%s: nothing matches the given filters\n", resolvedID)
	}
	return nil
}

// runColumn distinguishes the two rows that carry no run number: the stage log
// has none by nature, while a discovered file has one nobody can name.
func runColumn(f session.File) string {
	switch {
	case f.Run != session.UnknownRun:
		return fmt.Sprintf("%d", f.Run)
	case f.Artifact == session.ArtifactLog:
		return "-"
	}
	return "?"
}

func keep(f session.File, opts SessionsOptions) bool {
	if opts.Stage != "" && f.Stage != opts.Stage && f.Stage != opts.Stage+"-annotation" {
		return false
	}
	return opts.Run == nil || f.Run == *opts.Run
}

func artifacts(opts SessionsOptions) session.Artifacts {
	if opts.All {
		return session.Artifacts{Sessions: true, Logs: true, Events: true}
	}
	if !opts.Logs && !opts.Events {
		return session.Artifacts{Sessions: true}
	}
	return session.Artifacts{Logs: opts.Logs, Events: opts.Events}
}

// historyRuns reduces a ticket's history to what the resolver needs. The agent
// is the one the row recorded, falling back to the pipeline the ticket runs
// today only for a row that names none. A row needs the agent when it predates
// the session_ref field, and repointing the stage at another agent since then
// does not change which runtime wrote that run's files.
func historyRuns(cfg *config.Config, t *ticket.Ticket) []session.Run {
	runs := make([]session.Run, 0, len(t.History))
	for _, h := range t.History {
		agent := h.Agent
		if agent == "" {
			agent = app.AgentForStage(cfg, t.Pipeline, h.Stage)
		}
		runs = append(runs, session.Run{
			Stage: h.Stage,
			Index: h.Run,
			Agent: agent,
			Kind:  cfg.Agents[agent].Kind(),
			Ref:   session.Ref{Kind: h.SessionKind, Ref: h.SessionRef},
		})
	}
	return runs
}

// claudeConfigDir is where Claude keeps the projects directory this machine's
// sessions live under. The daemon resolves it per agent; here it is resolved
// once, from the global environment and then from the agents in name order, so
// a config that gives two Claude agents different config directories resolves
// against the first of them.
func claudeConfigDir(cfg *config.Config) string {
	if v := cfg.Environment["CLAUDE_CONFIG_DIR"]; v != "" {
		return v
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v := cfg.Agents[name].Environment["CLAUDE_CONFIG_DIR"]; v != "" {
			return v
		}
	}
	return session.ClaudeConfigDir(nil)
}
