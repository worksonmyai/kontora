package tmux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxPaneLogBytes = 16 << 20

func paneLogCommand(logPath string, scanPaths ...string) (string, error) {
	if len(scanPaths) > 1 {
		return "", fmt.Errorf("expected at most one PTY scan path")
	}
	if logPath != "" {
		var err error
		logPath, err = filepath.Abs(logPath)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil && len(scanPaths) == 0 {
			return "", fmt.Errorf("creating log directory: %w", err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating PTY log sink: %w", err)
	}
	command := "exec " + shellQuote(self) + " pty-log-sink " + shellQuote(logPath)
	if len(scanPaths) == 1 {
		scanPath, err := filepath.Abs(scanPaths[0])
		if err != nil {
			return "", err
		}
		command += " " + shellQuote(scanPath)
	}
	// pipe-pane expands tmux formats before the shell sees quoted paths.
	return strings.ReplaceAll(command, "#", "##"), nil
}

// DrainPaneLog consumes terminal output even when capture fills or fails,
// so logging cannot stall an agent.
func DrainPaneLog(input io.Reader, logPath string, scanPaths ...string) error {
	if len(scanPaths) > 1 {
		_, err := io.Copy(io.Discard, input)
		return errors.Join(fmt.Errorf("expected at most one PTY scan path"), err)
	}
	var scanner *paneLogScanner
	if len(scanPaths) == 1 {
		scanner = scanPaneLog(input, scanPaths[0])
		input = scanner
	}
	var captureErr error
	if logPath == "" && scanner != nil {
		_, captureErr = io.Copy(io.Discard, input)
	} else {
		f, remaining, err := openPaneLog(logPath)
		if err != nil {
			_, drainErr := io.Copy(io.Discard, input)
			captureErr = errors.Join(err, drainErr)
		} else {
			captureErr = drainPaneLog(input, f, remaining)
		}
	}
	if scanner != nil {
		return errors.Join(captureErr, scanner.finish())
	}
	return captureErr
}

func openPaneLog(logPath string) (*os.File, int64, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644)
	if err != nil {
		return nil, 0, err
	}
	// Concurrent sinks must not each claim the same remaining space.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, 0, errors.Join(err, f.Close())
	}
	info, err := f.Stat()
	if err != nil {
		return nil, 0, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.Join(fmt.Errorf("raw PTY log is not a regular file"), f.Close())
	}
	size := info.Size()
	if size > maxPaneLogBytes {
		if err := f.Truncate(maxPaneLogBytes); err != nil {
			return nil, 0, errors.Join(err, f.Close())
		}
		size = maxPaneLogBytes
	}
	return f, maxPaneLogBytes - size, nil
}

func drainPaneLog(input io.Reader, output io.WriteCloser, remaining int64) error {
	_, err := io.CopyN(output, input, remaining)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	err = errors.Join(err, output.Close())
	_, drainErr := io.Copy(io.Discard, input)
	return errors.Join(err, drainErr)
}
