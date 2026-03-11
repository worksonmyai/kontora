package cli

import "io"

// InitConfig runs the interactive setup wizard to create a config file.
func InitConfig(configPath string, w io.Writer) error {
	return runSetupFn(configPath, w)
}
