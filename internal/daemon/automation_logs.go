package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
)

func (d *Daemon) lockAutomationLogs() error {
	if d.automationLogLock != nil {
		return nil
	}
	if d.config().LogsDir == "" {
		return fmt.Errorf("%w: logs_dir is required", automation.ErrUnavailable)
	}
	root := filepath.Join(config.ExpandTilde(d.config().LogsDir), "automations")
	if err := automationPrivateDir(root); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".owner.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("%w: automation log namespace is already owned", automation.ErrUnavailable)
	}
	d.automationLogLock = lock
	return nil
}

func openAutomationArtifact(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(errors.New("run artifact is not a regular file"), f.Close())
	}
	return f, nil
}

func (d *Daemon) automationEvent(run automation.Run, kind, text string) error {
	path := filepath.Join(automationRunDir(d.config(), run), "lifecycle.jsonl")
	f, err := openAutomationArtifact(path)
	if err != nil {
		return err
	}
	return writeAutomationEvent(f, automation.LogLine{At: time.Now(), Source: "kontora", Kind: kind, Text: text})
}

func writeAutomationEvent(f interface {
	io.WriteCloser
	Sync() error
}, line automation.LogLine) (err error) {
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := json.NewEncoder(f).Encode(line); err != nil {
		return err
	}
	return f.Sync()
}

func (d *Daemon) AutomationLog(id string, n uint64) ([]automation.LogLine, string, error) {
	d.automationMu.Lock()
	if err := d.automationAvailableLocked(); err != nil {
		d.automationMu.Unlock()
		return nil, "", err
	}
	run, err := d.automationStore.GetRun(id, n)
	d.automationMu.Unlock()
	if err != nil {
		return nil, "", err
	}
	dir := automationRunDir(d.config(), run)
	lines := []automation.LogLine{}
	b, err := readAutomationArtifact(filepath.Join(dir, "lifecycle.jsonl"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	for {
		var line automation.LogLine
		err := dec.Decode(&line)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("reading run lifecycle: %w", err)
		}
		lines = append(lines, line)
	}
	var tape logfmt.Tape
	b, err = readAutomationArtifact(filepath.Join(dir, "events.json"))
	if err == nil {
		if err := json.Unmarshal(b, &tape); err != nil {
			return nil, "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	for _, event := range tape.Events {
		at := time.Time{}
		if event.Time != nil {
			at = *event.Time
		}
		switch event.Kind {
		case "text":
			lines = append(lines, automation.LogLine{At: at, Source: "agent", Kind: "text", Text: event.Text})
		case "tool":
			lines = append(lines, automation.LogLine{At: at, Source: "agent", Kind: "tool_call", Text: event.Tool + " " + event.Arg})
			if event.Result != "" {
				kind := "tool_result"
				if event.IsError {
					kind = "error"
				}
				lines = append(lines, automation.LogLine{At: at, Source: "agent", Kind: kind, Text: event.Result})
			}
		}
	}
	if len(tape.Events) == 0 {
		b, err = readAutomationArtifact(filepath.Join(dir, "capture.log"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, "", err
		}
		scanner := bufio.NewScanner(strings.NewReader(string(b)))
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			lines = append(lines, automation.LogLine{Source: "agent", Kind: "text", Text: scanner.Text()})
		}
		if err := scanner.Err(); err != nil {
			return nil, "", err
		}
	}
	if len(lines) == 0 && run.Outcome == "skipped" {
		lines = append(lines, automation.LogLine{At: run.AcceptedAt, Source: "kontora", Kind: "lifecycle", Text: run.Result})
	}
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].At.IsZero() || lines[j].At.IsZero() {
			return !lines[i].At.IsZero() && lines[j].At.IsZero()
		}
		return lines[i].At.Before(lines[j].At)
	})
	var raw strings.Builder
	for _, line := range lines {
		stamp := ""
		if !line.At.IsZero() {
			stamp = line.At.Format(time.RFC3339) + " "
		}
		fmt.Fprintf(&raw, "%s%-7s %s\n", stamp, line.Source, line.Text)
	}
	return lines, raw.String(), nil
}

func readAutomationArtifact(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("run artifact is not a regular file")
	}
	if info.Size() > 16<<20 {
		return nil, errors.New("run artifact exceeds 16 MiB; inspect the file on the daemon host")
	}
	return io.ReadAll(f)
}
