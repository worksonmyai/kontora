package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// fallbackBinDirs lists directories kontora consults when a binary isn't on
// $PATH. Daemons launched from launchd / macOS Login Items inherit a stripped
// PATH that omits these common install locations, so a user's `claude` or
// `plannotator` on ~/.local/bin is invisible to the daemon without this
// fallback. The list is intentionally small and platform-agnostic: the same
// paths work on macOS and Linux.
var fallbackBinDirs = []string{
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"/usr/bin",
	"/bin",
}

// LookupBinary resolves binary to an absolute path. It tries, in order:
//  1. the absolute path as given
//  2. $PATH via exec.LookPath
//  3. ~/.local/bin and the paths in fallbackBinDirs
//
// On failure the error names the directories that were searched so operators
// can see where to install the binary.
func LookupBinary(binary string) (string, error) {
	if binary == "" {
		return "", errors.New("binary is empty")
	}
	if filepath.IsAbs(binary) {
		if _, err := os.Stat(binary); err != nil {
			return "", err
		}
		return binary, nil
	}
	if p, err := exec.LookPath(binary); err == nil {
		return p, nil
	}
	for _, c := range candidatePaths(binary) {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("%q not found in $PATH or %v", binary, searchedDirs())
}

func candidatePaths(binary string) []string {
	paths := make([]string, 0, len(fallbackBinDirs)+1)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".local", "bin", binary))
	}
	for _, dir := range fallbackBinDirs {
		paths = append(paths, filepath.Join(dir, binary))
	}
	return paths
}

func searchedDirs() []string {
	dirs := make([]string, 0, len(fallbackBinDirs)+1)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	dirs = append(dirs, fallbackBinDirs...)
	return dirs
}
