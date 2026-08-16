package cli

import (
	"io"

	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/config"
)

// RedactedToken is what ShowConfig prints in place of a set web token.
const RedactedToken = "<redacted>"

// ShowConfig writes the effective configuration (with defaults applied) as
// YAML. The web token is replaced with a placeholder: this output is what
// people paste into issues and chat threads, and the token is the only thing
// gating remote access to the daemon. `kontora config edit` shows the real
// file when the token itself needs checking.
func ShowConfig(cfg *config.Config, w io.Writer) error {
	if cfg.Web.Token != "" {
		redacted := *cfg
		redacted.Web.Token = RedactedToken
		cfg = &redacted
	}

	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return enc.Close()
}
