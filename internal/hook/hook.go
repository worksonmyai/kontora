// Package hook runs the shell commands a user configures at a ticket's
// lifecycle boundaries.
package hook

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/worksonmyai/kontora/internal/process"
)

// Spec is one command to run at a lifecycle event.
type Spec struct {
	// Name labels the hook in logs and errors. A hook without one is labelled
	// by its event and position in the sequence.
	Name    string
	Run     string
	Timeout time.Duration
	// Fatal marks a hook whose failure stops the sequence: the resolved
	// on_failure policy is pause rather than warn.
	Fatal bool
}

// Context is what a hook is told about the run it belongs to. Every field is
// passed as KONTORA_<FIELD>, with ExitCode omitted while nil so that only a
// post-run event sets KONTORA_EXIT_CODE.
type Context struct {
	Event      string
	TicketID   string
	TicketFile string
	Worktree   string
	RepoPath   string
	Branch     string
	Stage      string
	Agent      string
	Project    string
	ExitCode   *int
}

func (c Context) env() []string {
	env := []string{
		"KONTORA_EVENT=" + c.Event,
		"KONTORA_TICKET_ID=" + c.TicketID,
		"KONTORA_TICKET_FILE=" + c.TicketFile,
		"KONTORA_WORKTREE=" + c.Worktree,
		"KONTORA_REPO_PATH=" + c.RepoPath,
		"KONTORA_BRANCH=" + c.Branch,
		"KONTORA_STAGE=" + c.Stage,
		"KONTORA_AGENT=" + c.Agent,
		"KONTORA_PROJECT=" + c.Project,
	}
	if c.ExitCode != nil {
		env = append(env, "KONTORA_EXIT_CODE="+strconv.Itoa(*c.ExitCode))
	}
	return env
}

// Run executes specs in order, appending the combined output of each to out. It
// stops at the first Fatal spec that fails and returns its error; the failure of
// a non-fatal spec is reported through onWarn and the sequence continues. A
// cancelled ctx stops the sequence whatever the policy, and its error is
// returned so the caller can tell a cancellation from a hook that went wrong.
func Run(ctx context.Context, specs []Spec, hc Context, out io.Writer, onWarn func(Spec, error)) error {
	env := hc.env()
	for i, spec := range specs {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := label(hc.Event, spec, i)
		err := runOne(ctx, spec, hc, env, out, name)
		if err == nil {
			continue
		}
		err = fmt.Errorf("hook %s: %w", name, err)
		if spec.Fatal || ctx.Err() != nil {
			return err
		}
		if onWarn != nil {
			onWarn(spec, err)
		}
	}
	return nil
}

func runOne(ctx context.Context, spec Spec, hc Context, env []string, out io.Writer, name string) error {
	// Every hook of every event of a ticket appends to one file, so each run
	// announces itself: without this the output of one hook cannot be told from
	// the next one's.
	fmt.Fprintf(out, "\n=== %s %s %s ===\n", time.Now().UTC().Format(time.RFC3339), hc.Event, name)

	runCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	// The timeout is left to runCtx rather than process.RunParams.Timeout so
	// that a killed command is reported as a timeout instead of as the exit
	// code its termination signal produces. Both routes terminate the same way.
	result, err := process.Run(runCtx, process.RunParams{
		Binary: "/bin/sh",
		Args:   []string{"-c", spec.Run},
		Dir:    hc.Worktree,
		Stdout: out,
		Stderr: out,
		Env:    env,
	})
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("timed out after %s", spec.Timeout)
	case err != nil:
		return err
	case result.ExitCode != 0:
		return fmt.Errorf("exited with code %d", result.ExitCode)
	}
	return nil
}

// label identifies a hook in logs and errors. A hook that omits its name is
// named by event and position, the form the config validation errors use.
func label(event string, spec Spec, index int) string {
	if spec.Name != "" {
		return spec.Name
	}
	return fmt.Sprintf("%s[%d]", event, index)
}
