package cli

import (
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
)

// Doctor runs a series of checks against the kontora setup and prints
// results as colored status indicators. Returns an error if any hard
// prerequisite fails.
func Doctor(configPath string, w io.Writer) error {
	var hasFail bool
	var warnings int

	check := func(level, name, detail string) {
		printCheck(w, level, name, detail)
		switch level {
		case "FAIL":
			hasFail = true
		case "WARN":
			warnings++
		}
	}

	fmt.Fprintln(w, styleBold.Render("Checking kontora setup..."))
	fmt.Fprintln(w)

	// 1. Config file exists and parses.
	cfg, err := config.Load(configPath)
	if err != nil {
		check("FAIL", "Config", err.Error())
		cfg = nil
	} else {
		check("OK", "Config", configPath)
	}

	// 2. Directories (warn only — auto-created on use).
	if cfg != nil {
		for _, dir := range []struct {
			name string
			path string
		}{
			{"Tickets dir", config.ExpandTilde(cfg.TicketsDir)},
			{"Logs dir", config.ExpandTilde(cfg.LogsDir)},
			{"Worktrees dir", config.ExpandTilde(cfg.WorktreesDir)},
		} {
			if info, err := os.Stat(dir.path); err == nil && info.IsDir() {
				check("OK", dir.name, dir.path)
			} else {
				check("WARN", dir.name, fmt.Sprintf("%s (will be auto-created)", dir.path))
			}
		}
	}

	// 3. Required tools.
	for _, tool := range []string{"git", "tmux"} {
		if _, err := exec.LookPath(tool); err != nil {
			check("FAIL", tool, "not found on PATH")
		} else {
			check("OK", tool, "found")
		}
	}

	// 4. Agent binaries.
	if cfg != nil {
		for _, name := range slices.Sorted(maps.Keys(cfg.Agents)) {
			agent := cfg.Agents[name]
			label := fmt.Sprintf("Agent %q (%s)", name, agent.Binary)
			resolved, err := process.LookupBinary(agent.Binary)
			if err != nil {
				check("FAIL", label, err.Error())
			} else {
				check("OK", label, resolved)
			}
		}
	}

	// 5. Project repositories. A path that is not a git worktree cannot have a
	// branch cut from it, so every ticket for that project pauses at pickup.
	if cfg != nil {
		for _, name := range slices.Sorted(maps.Keys(cfg.Projects)) {
			path := config.ExpandTilde(cfg.Projects[name].Path)
			label := fmt.Sprintf("Project %q", name)
			switch info, err := os.Stat(path); {
			case err != nil:
				check("FAIL", label, fmt.Sprintf("%s does not exist", path))
			case !info.IsDir():
				check("FAIL", label, fmt.Sprintf("%s is not a directory", path))
			case !isGitRepo(path):
				check("FAIL", label, fmt.Sprintf("%s is not a git repository", path))
			default:
				check("OK", label, path)
			}
		}
	}

	// 6. Plannotator (warn only — it is needed for `review` and `annotate`, not
	// for running agents).
	if cfg != nil && cfg.Plannotator.Binary != "" {
		label := fmt.Sprintf("Plannotator (%s)", cfg.Plannotator.Binary)
		if resolved, err := process.LookupBinary(cfg.Plannotator.Binary); err != nil {
			check("WARN", label, "not found on PATH (only needed for review/annotate)")
		} else {
			check("OK", label, resolved)
		}
	}

	// 7. Web port availability (warn only).
	if cfg != nil && cfg.Web.Enabled != nil && *cfg.Web.Enabled {
		addr := net.JoinHostPort(cfg.Web.Host, strconv.Itoa(cfg.Web.Port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			check("WARN", "Web port", fmt.Sprintf("%s is not available (%v)", addr, err))
		} else {
			ln.Close()
			check("OK", "Web port", fmt.Sprintf("%s is available", addr))
		}
	}

	fmt.Fprintln(w)
	switch {
	case hasFail:
		fmt.Fprintln(w, styleFail.Render("Some checks failed."))
		return fmt.Errorf("one or more checks failed")
	case warnings == 1:
		fmt.Fprintln(w, styleWarn.Render("All checks passed, with 1 warning."))
	case warnings > 1:
		fmt.Fprintln(w, styleWarn.Render(fmt.Sprintf("All checks passed, with %d warnings.", warnings)))
	default:
		fmt.Fprintln(w, styleOK.Render("All checks passed."))
	}
	return nil
}

// isGitRepo reports whether path is inside a git working tree.
func isGitRepo(path string) bool {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree")
	return cmd.Run() == nil
}

func printCheck(w io.Writer, level, name, detail string) {
	var symbol string
	switch level {
	case "OK":
		symbol = styleOK.Render("✓")
	case "WARN":
		symbol = styleWarn.Render("!")
	case "FAIL":
		symbol = styleFail.Render("✗")
	}
	fmt.Fprintf(w, "  %s %-30s %s\n", symbol, name, styleFaint.Render(detail))
}
