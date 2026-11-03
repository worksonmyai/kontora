package session

import (
	"fmt"
	"path/filepath"
	"strings"
)

func AttemptDir(logsDir, attemptID string) (string, error) {
	if !SafeStage(attemptID) || strings.ContainsAny(attemptID, "*?[]:\x00\n\r") {
		return "", fmt.Errorf("invalid attempt identity %q", attemptID)
	}
	return filepath.Join(logsDir, "attempts", attemptID), nil
}

func AttemptLogPath(logsDir, attemptID string) (string, error) {
	dir, err := AttemptDir(logsDir, attemptID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "output.log"), nil
}

func AttemptWaitPath(logsDir, attemptID string) (string, error) {
	dir, err := AttemptDir(logsDir, attemptID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "waiting.json"), nil
}
