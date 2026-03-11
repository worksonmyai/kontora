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
}

type Result struct {
	ExitCode  int
	StartedAt time.Time
	ExitedAt  time.Time
}

func Run(ctx context.Context, params RunParams) (Result, error) {
	cmd := exec.Command(params.Binary, params.Args...)
	cmd.Dir = params.Dir
	cmd.Stdout = params.Stdout
	cmd.Stderr = params.Stderr
	if len(params.Env) > 0 {
		cmd.Env = append(os.Environ(), params.Env...)
	}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("starting process: %w", err)
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

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-timeoutCh:
		stop := terminate(cmd)
		waitErr = <-waitCh
		stop()
	case <-ctx.Done():
		stop := terminate(cmd)
		waitErr = <-waitCh
		stop()
	}

	exitedAt := time.Now()

	result := Result{
		ExitCode:  exitCode(waitErr),
		StartedAt: startedAt,
		ExitedAt:  exitedAt,
	}
	if waitErr != nil && result.ExitCode == -1 {
		return result, waitErr
	}
	return result, nil
}

// terminate sends SIGTERM and schedules a SIGKILL after gracePeriod.
// The returned function cancels the pending SIGKILL and must be called
// after cmd.Wait() returns to avoid killing a recycled PID.
func terminate(cmd *exec.Cmd) (cancel func()) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(gracePeriod):
			_ = cmd.Process.Kill()
		case <-done:
		}
	}()
	return func() { close(done) }
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				return 128 + int(status.Signal())
			}
		}
		return exitErr.ExitCode()
	}
	return -1
}
