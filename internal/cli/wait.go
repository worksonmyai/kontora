package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/worksonmyai/kontora/internal/session"
)

type WaitOptions struct {
	Check    string
	Poll     string
	Deadline string
	Until    string
	Next     string
	Summary  string
}

// ReportWait publishes a suspension request for the environment-bound invocation.
// Acceptance confirms publication, not daemon commitment of the suspension.
func ReportWait(opts WaitOptions, out io.Writer, getenv func(string) string) error {
	inv, err := session.InvocationFromEnv(getenv)
	if err != nil {
		return err
	}
	if inv.Version != session.WaitResultVersion {
		return fmt.Errorf("wait requires result version 3")
	}
	if (opts.Check == "") == (opts.Until == "") {
		return fmt.Errorf("exactly one of --check and --until is required")
	}
	request := &session.WaitRequest{}
	if opts.Until != "" {
		if opts.Poll != "" || opts.Deadline != "" {
			return fmt.Errorf("--poll and --deadline are only valid with --check")
		}
		request.Kind = "timer"
		request.Until, err = time.Parse(time.RFC3339Nano, opts.Until)
		if err != nil {
			return fmt.Errorf("--until must be an absolute RFC 3339 time: %w", err)
		}
	} else {
		request.Kind = "check"
		request.Command = opts.Check
		request.Poll, err = time.ParseDuration(opts.Poll)
		if err != nil || request.Poll <= 0 {
			return fmt.Errorf("--poll must be a positive Go duration")
		}
		request.Deadline, err = time.Parse(time.RFC3339Nano, opts.Deadline)
		if err != nil {
			return fmt.Errorf("--deadline must be an absolute RFC 3339 time: %w", err)
		}
	}
	summary := opts.Summary
	if summary == "" {
		if request.Kind == "timer" {
			summary = "Waiting for timer."
		} else {
			summary = "Waiting for condition check."
		}
	}
	return publishInvocationResult(inv, session.Result{
		Outcome: "waiting", Summary: summary, NextStep: opts.Next, Wait: request,
	}, out)
}
