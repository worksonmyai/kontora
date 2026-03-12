package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// ResolveConfigPath finds the config file by checking the local .kontora
// directory first, then each config dir for kontora/config.yaml.
func ResolveConfigPath(workDir string, configDirs []string) string {
	local := filepath.Join(workDir, ".kontora", "config.yaml")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	for _, dir := range configDirs {
		p := filepath.Join(dir, "kontora", "config.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if len(configDirs) > 0 {
		return filepath.Join(configDirs[0], "kontora", "config.yaml")
	}
	return local
}

// DefaultConfigPath returns the default config file path, checking the
// current working directory and standard config directories.
func DefaultConfigPath() string {
	wd, err := os.Getwd()
	if err != nil {
		return filepath.Join(".kontora", "config.yaml")
	}
	return ResolveConfigPath(wd, configDirs())
}

func configDirs() []string {
	var dirs []string
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		dirs = append(dirs, xdg)
	} else if runtime.GOOS != "darwin" {
		if d, err := os.UserConfigDir(); err == nil {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, ".config"))
		}
	}
	return dirs
}
