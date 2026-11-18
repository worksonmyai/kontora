package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/worksonmyai/kontora/internal/session"
)

// prepareRawCapture gives the runner a new file and publishes that file under
// the old stage name for CLI and Activity clients. The next run replaces the
// stage link without changing the capture held by this run.
func prepareRawCapture(logsDir, ticketID, stage, sharedPath string) (id, path string, err error) {
	if !session.ValidLogStage(stage) {
		return "", "", fmt.Errorf("invalid log stage %q", stage)
	}
	id = newSessionID()
	path = session.RawLogPath(logsDir, ticketID, id, stage)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", "", err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", "", err
	}
	if err := linkRawCapture(path, sharedPath); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return id, path, nil
}

func captureRunnerParams(log *slog.Logger, logsDir, ticketID, stage string, params RunnerParams, graph bool) (RunnerParams, string) {
	if graph {
		return params, ""
	}
	id, path, err := prepareRawCapture(expandTilde(logsDir), ticketID, stage, params.LogFile)
	if err != nil {
		log.Warn("prepare exact log capture", "err", err)
	}
	// A failed setup must not append to the link to the preceding capture.
	params.LogFile = path
	return params, id
}

func logDetectionParams(params, capture RunnerParams) RunnerParams {
	if capture.LogFile == "" {
		return capture
	}
	return params
}

func linkRawCapture(path, sharedPath string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	link := filepath.Join(filepath.Dir(path), "latest")
	if err := os.Link(path, link); err != nil {
		return err
	}
	return os.Rename(link, sharedPath)
}
