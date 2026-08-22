package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// CommonBinDirs is the directory list LookupBinary falls back to, in the order
// it searches them: ~/.local/bin first, then fallbackBinDirs. Callers that
// spawn a process hand these to it as well, so a wrapper binary such as `nono`
// resolves the agent behind its `--` the same way kontora resolved the wrapper.
func CommonBinDirs() []string {
	dirs := make([]string, 0, len(fallbackBinDirs)+1)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"))
	}
	return append(dirs, fallbackBinDirs...)
}

// PathWith returns current with each of dirs appended that it does not already
// contain. Appending rather than prepending keeps the precedence LookupBinary
// has: what is already on $PATH wins, and these dirs only fill the gap.
func PathWith(current string, dirs ...string) string {
	have := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	if current != "" {
		out = append(out, current)
		for _, p := range filepath.SplitList(current) {
			have[p] = true
		}
	}
	for _, dir := range dirs {
		if dir == "" || have[dir] {
			continue
		}
		have[dir] = true
		out = append(out, dir)
	}
	return strings.Join(out, string(filepath.ListSeparator))
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
	return "", fmt.Errorf("%q not found in $PATH or %v", binary, CommonBinDirs())
}

func candidatePaths(binary string) []string {
	dirs := CommonBinDirs()
	paths := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(dir, binary))
	}
	return paths
}
