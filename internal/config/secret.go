package config

import (
	"fmt"
	"os"
	"strings"
)

// ResolveSecret reads the credential a notification channel names. It lives
// here so the daemon, which builds the channel, and `kontora doctor`, which
// predicts whether it will build, answer the question the same way: the two had
// drifted, and doctor reported a file holding one newline as fine.
//
// Nothing stores the result: no token enters Config, which is what `kontora
// config` prints and what a reload re-decodes.
//
// An empty result is not an error here. Only telegram and mattermost cannot
// work without a credential, and validateNotifications has already refused one
// of those that named no source.
func (c NotifyChannel) ResolveSecret() (string, error) {
	switch {
	case c.SecretEnv != "":
		v := os.Getenv(c.SecretEnv)
		if v == "" {
			return "", fmt.Errorf("$%s is empty or unset", c.SecretEnv)
		}
		return v, nil
	case c.SecretFile != "":
		path := ExpandTilde(c.SecretFile)
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", path, err)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return v, nil
	default:
		return "", nil
	}
}

// SecretSource names where a channel's credential comes from, for a log line or
// a doctor row. It is the name of the source, never the value.
func (c NotifyChannel) SecretSource() string {
	switch {
	case c.SecretEnv != "":
		return "$" + c.SecretEnv
	case c.SecretFile != "":
		return c.SecretFile
	default:
		return "none"
	}
}
