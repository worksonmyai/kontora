package process

import (
	"fmt"
	"os"
	"strings"
)

// WorkerEnvironment merges the actual inherited environment with overrides,
// then removes shared web credentials and the configured operator-secret keys.
// Use the result as a complete replacement: RunParams.ReplaceEnv must be true,
// or exec.Cmd.Env must be set to it directly. Appending it to os.Environ would
// restore the secrets. This is not a sandbox or an environment allowlist.
func WorkerEnvironment(overrides, operatorSecretKeys []string) ([]string, error) {
	return SanitizeEnvironment(os.Environ(), overrides, operatorSecretKeys)
}

// SanitizeEnvironment is the explicit-base form of WorkerEnvironment. Filtering
// applies to both inputs, so an override cannot reintroduce a blocked key. It
// returns a non-nil slice even when empty, preventing exec.Cmd's nil inheritance.
// Callers must name every custom operator-secret key, including aliases.
func SanitizeEnvironment(inherited, overrides, operatorSecretKeys []string) ([]string, error) {
	blocked := map[string]bool{
		"KONTORA_TOKEN":          true,
		"KONTORA_WEB_TOKEN":      true,
		"KONTORA_OPERATOR_TOKEN": true,
	}
	for _, key := range operatorSecretKeys {
		if !validEnvironmentKey(key) {
			return nil, fmt.Errorf("invalid operator-secret environment key")
		}
		blocked[key] = true
	}

	result := make([]string, 0, len(inherited)+len(overrides))
	positions := make(map[string]int)
	for _, entries := range [][]string{inherited, overrides} {
		for _, entry := range entries {
			key, _, ok := strings.Cut(entry, "=")
			if !ok || !validEnvironmentKey(key) || strings.ContainsRune(entry, '\x00') {
				return nil, fmt.Errorf("invalid environment entry")
			}
			if blocked[key] {
				continue
			}
			if position, exists := positions[key]; exists {
				result[position] = entry
			} else {
				positions[key] = len(result)
				result = append(result, entry)
			}
		}
	}
	return result, nil
}

func validEnvironmentKey(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}
