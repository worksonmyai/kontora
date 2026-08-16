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

// PathEnvVar names the config file for every kontora command in a process
// tree. The daemon exports it to the agents it spawns, so `kontora note` inside
// a worktree writes to the same config the daemon was started with, rather than
// re-deriving one from the agent's working directory and $HOME.
const PathEnvVar = "KONTORA_CONFIG"

// DefaultConfigPath returns the default config file path: the one KONTORA_CONFIG
// names, else the first hit when checking the current working directory and the
// standard config directories.
func DefaultConfigPath() string {
	if p := os.Getenv(PathEnvVar); p != "" {
		return ExpandTilde(p)
	}
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
