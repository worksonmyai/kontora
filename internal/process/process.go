package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const gracePeriod = 10 * time.Second

type RunParams struct {
	Binary  string
	Args    []string
	Dir     string
	Timeout time.Duration
	Stdout  io.Writer
	Stderr  io.Writer
	Env     []string // additional KEY=VALUE pairs appended to os.Environ()
	// ReplaceEnv uses only Env, including an empty environment when Env is nil.
	ReplaceEnv         bool
	RequireTermination bool
}

// TerminationEvidence describes what the runner verified, not just what it signaled.
type TerminationEvidence string

const (
	TerminationUnknown          TerminationEvidence = ""
	TerminationNotStarted       TerminationEvidence = "not_started"
	TerminationProcessGroupGone TerminationEvidence = "process_group_gone"
	// TerminationWindowGone does not prove that detached descendants stopped.
	TerminationWindowGone TerminationEvidence = "tmux_window_gone"
)

var ErrTerminationUncertain = errors.New("termination could not be confirmed")

type Result struct {
	ExitCode  int
	StartedAt time.Time
	// ExitedAt records when the runner stopped waiting, including uncertain termination.
	ExitedAt    time.Time
	Termination TerminationEvidence
	// Interruption records cancellation or timeout, even when the process exits zero.
	Interruption error
}

func Run(ctx context.Context, params RunParams) (Result, error) {
	cmd := exec.Command(params.Binary, params.Args...)
	cmd.Dir = params.Dir
	cmd.Stdout = params.Stdout
	cmd.Stderr = params.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Bound pipe draining if a descendant keeps an inherited descriptor open.
	cmd.WaitDelay = gracePeriod + time.Second
	if params.ReplaceEnv {
		cmd.Env = append([]string{}, params.Env...)
	} else if len(params.Env) > 0 {
		cmd.Env = append(os.Environ(), params.Env...)
	}

	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1, ExitedAt: time.Now(), Termination: TerminationNotStarted, Interruption: err}, err
	}

	if err := cmd.Start(); err != nil {
		return Result{ExitCode: -1, ExitedAt: time.Now(), Termination: TerminationNotStarted}, fmt.Errorf("starting process: %w", err)
	}

	startedAt := time.Now()

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var timeoutCh <-chan time.Time
	if params.Timeout > 0 {
		timer := time.NewTimer(params.Timeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	var waitErr, stopErr, interruption error
	termination := TerminationUnknown
	stop := func() {
		stopErr = terminateGroup(cmd.Process.Pid, gracePeriod, time.Second, syscall.Kill)
		if stopErr != nil {
			select {
			case waitErr = <-waitCh:
			default:
				waitErr = stopErr
			}
			return
		}
		termination = TerminationProcessGroupGone
		waitErr = <-waitCh
	}
	select {
	case waitErr = <-waitCh:
		if params.RequireTermination {
			stopErr = terminateGroup(cmd.Process.Pid, gracePeriod, time.Second, syscall.Kill)
			if stopErr == nil {
				termination = TerminationProcessGroupGone
			}
		}
	case <-timeoutCh:
		interruption = context.DeadlineExceeded
		stop()
	case <-ctx.Done():
		interruption = ctx.Err()
		stop()
	}

	exitedAt := time.Now()

	result := Result{
		ExitCode:     exitCode(waitErr),
		StartedAt:    startedAt,
		ExitedAt:     exitedAt,
		Termination:  termination,
		Interruption: interruption,
	}
	if stopErr != nil {
		return result, errors.Join(interruption, stopErr)
	}
	if waitErr != nil && result.ExitCode == -1 {
		return result, waitErr
	}
	return result, nil
}

// A reaped leader does not mean its descendants exited. Keep checking the group
// through escalation, and report uncertainty if even SIGKILL cannot remove it.
func terminateGroup(pgid int, grace, confirmation time.Duration, kill func(int, syscall.Signal) error) error {
	signal := func(sig syscall.Signal) error {
		err := kill(-pgid, sig)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if err := signal(syscall.SIGTERM); err != nil {
		return errors.Join(ErrTerminationUncertain, fmt.Errorf("sending process group SIGTERM: %w", err))
	}
	deadline := time.Now().Add(grace)
	escalated := false
	for {
		err := kill(-pgid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return errors.Join(ErrTerminationUncertain, fmt.Errorf("inspecting process group: %w", err))
		}
		if !time.Now().Before(deadline) {
			if escalated {
				return fmt.Errorf("%w: process group %d still exists after SIGKILL", ErrTerminationUncertain, pgid)
			}
			if err := signal(syscall.SIGKILL); err != nil {
				return errors.Join(ErrTerminationUncertain, fmt.Errorf("sending process group SIGKILL: %w", err))
			}
			escalated = true
			deadline = time.Now().Add(confirmation)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
		}
		return exitErr.ExitCode()
	}
	return -1
}
