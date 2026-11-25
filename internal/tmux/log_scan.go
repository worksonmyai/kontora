package tmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// tmux can queue output faster than MatchReader consumes it. Allow the sink to finish that backlog after exit.
const paneLogScanTimeout = 30 * time.Second

type paneLogScanSpec struct {
	Patterns []string `json:"patterns"`
}

type paneLogScanResult struct {
	Complete       bool    `json:"complete"`
	FailurePattern *string `json:"failure_pattern,omitempty"`
	Error          string  `json:"error,omitempty"`
}

type paneLogScan struct {
	path     string
	patterns []string
	window   *identityTmux
}

func newPaneLogScan(patterns []string) (*paneLogScan, error) {
	for _, pattern := range patterns {
		if _, err := regexp.Compile(pattern); err != nil {
			return nil, fmt.Errorf("compiling failure pattern: %w", err)
		}
	}
	dir, err := os.MkdirTemp("", "kontora-scan-*")
	if err != nil {
		return nil, err
	}
	scan := &paneLogScan{path: filepath.Join(dir, "spec.json"), patterns: append([]string(nil), patterns...)}
	data, err := json.Marshal(paneLogScanSpec{Patterns: patterns})
	if err == nil {
		err = os.WriteFile(scan.path, data, 0o600)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return scan, nil
}

func (s *paneLogScan) close() {
	if s.window != nil {
		// A failed command may leave a debug shell alive. Close only its captured pipe, not a replacement window's.
		_ = s.window.run("pipe-pane", "-t", s.window.id.WindowID)
	}
	_ = os.RemoveAll(filepath.Dir(s.path))
}

func (s *paneLogScan) wait(ctx context.Context) (*string, error) {
	ctx, cancel := context.WithTimeout(ctx, paneLogScanTimeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(s.path + ".result")
		if err == nil {
			var result paneLogScanResult
			if err := json.Unmarshal(data, &result); err != nil {
				return nil, fmt.Errorf("reading PTY failure scan: %w", err)
			}
			if !result.Complete || result.Error != "" {
				return nil, fmt.Errorf("PTY failure scan incomplete: %s", result.Error)
			}
			if result.FailurePattern != nil {
				found := false
				for _, pattern := range s.patterns {
					found = found || pattern == *result.FailurePattern
				}
				if !found {
					return nil, fmt.Errorf("PTY failure scan returned an unconfigured pattern")
				}
			}
			return result.FailurePattern, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reading PTY failure scan: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for PTY failure scan completion: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

type patternScanResult struct {
	matched bool
	err     error
}

type patternScanner struct {
	input  *io.PipeWriter
	result <-chan patternScanResult
}

type paneLogScanner struct {
	input    io.Reader
	path     string
	patterns []string
	scanners []patternScanner
	err      error
	eof      bool
}

func scanPaneLog(input io.Reader, path string) *paneLogScanner {
	s := &paneLogScanner{input: input, path: path}
	data, err := os.ReadFile(path)
	var spec paneLogScanSpec
	if err == nil {
		err = json.Unmarshal(data, &spec)
	}
	if err != nil {
		s.err = fmt.Errorf("reading PTY scan spec: %w", err)
		return s
	}
	s.patterns = spec.Patterns
	for _, pattern := range s.patterns {
		rx, err := regexp.Compile(pattern)
		if err != nil {
			s.err = errors.Join(s.err, err)
			break
		}
		reader, writer := io.Pipe()
		done := make(chan patternScanResult, 1)
		s.scanners = append(s.scanners, patternScanner{input: writer, result: done})
		go func() {
			defer reader.Close()
			buffer := bufio.NewReader(reader)
			matched := rx.MatchReader(buffer)
			// MatchReader may return before EOF. Keep consuming so a match cannot block the pipe fanout.
			_, err := io.Copy(io.Discard, buffer)
			done <- patternScanResult{matched: matched, err: err}
		}()
	}
	return s
}

func (s *paneLogScanner) Read(p []byte) (int, error) {
	n, err := s.input.Read(p)
	for i, scanner := range s.scanners {
		if scanner.input == nil {
			continue
		}
		if _, writeErr := scanner.input.Write(p[:n]); writeErr != nil {
			s.err = errors.Join(s.err, writeErr)
			_ = scanner.input.Close()
			s.scanners[i].input = nil
		}
	}
	if errors.Is(err, io.EOF) {
		s.eof = true
	} else if err != nil {
		s.err = errors.Join(s.err, err)
	}
	return n, err
}

func (s *paneLogScanner) finish(captureErr error) error {
	for _, scanner := range s.scanners {
		if scanner.input != nil {
			_ = scanner.input.Close()
		}
	}
	result := paneLogScanResult{}
	for i, scanner := range s.scanners {
		scanned := <-scanner.result
		s.err = errors.Join(s.err, scanned.err)
		if scanned.matched && result.FailurePattern == nil {
			result.FailurePattern = new(s.patterns[i])
		}
	}
	if !s.eof && s.err == nil {
		s.err = io.ErrUnexpectedEOF
	}
	resultErr := s.err
	// Without patterns, this acknowledgement covers the capture itself.
	if len(s.patterns) == 0 {
		resultErr = errors.Join(resultErr, captureErr)
	}
	result.Complete = resultErr == nil
	if resultErr != nil {
		result.Error = resultErr.Error()
	}
	return errors.Join(s.err, publishPaneLogScan(s.path, result))
}

func publishPaneLogScan(path string, result paneLogScanResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".scan-result-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path+".result")
}
