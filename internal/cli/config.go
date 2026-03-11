package cli

import (
	"io"

	"github.com/worksonmyai/kontora/internal/config"
	"gopkg.in/yaml.v3"
)

// ShowConfig writes the effective configuration (with defaults applied) as YAML.
func ShowConfig(cfg *config.Config, w io.Writer) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return enc.Close()
}
