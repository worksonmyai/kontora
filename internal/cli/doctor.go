package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"

	"github.com/worksonmyai/kontora/internal/config"
)

// Doctor runs a series of checks against the kontora setup and prints
// results as colored status indicators. Returns an error if any hard
// prerequisite fails.
func Doctor(configPath string, w io.Writer) error {
	var hasFail bool

	fmt.Fprintln(w, styleBold.Render("Checking kontora setup..."))
	fmt.Fprintln(w)

	// 1. Config file exists and parses.
	cfg, err := config.Load(configPath)
	if err != nil {
		printCheck(w, "FAIL", "Config", err.Error())
		hasFail = true
		cfg = nil
	} else {
		printCheck(w, "OK", "Config", configPath)
	}

	// 2. Directories (warn only — auto-created on use).
	if cfg != nil {
		for _, check := range []struct {
			name string
			path string
		}{
			{"Tickets dir", config.ExpandTilde(cfg.TicketsDir)},
			{"Logs dir", config.ExpandTilde(cfg.LogsDir)},
		} {
			if info, err := os.Stat(check.path); err == nil && info.IsDir() {
				printCheck(w, "OK", check.name, check.path)
			} else {
				printCheck(w, "WARN", check.name, fmt.Sprintf("%s (will be auto-created)", check.path))
			}
		}
	}

	// 3. Required tools.
	for _, tool := range []string{"git", "tmux"} {
		if _, err := exec.LookPath(tool); err != nil {
			printCheck(w, "FAIL", tool, "not found on PATH")
			hasFail = true
		} else {
			printCheck(w, "OK", tool, "found")
		}
	}

	// 4. Agent binaries.
	if cfg != nil {
		agentNames := make([]string, 0, len(cfg.Agents))
		for name := range cfg.Agents {
			agentNames = append(agentNames, name)
		}
		slices.Sort(agentNames)
		for _, name := range agentNames {
			agent := cfg.Agents[name]
			label := fmt.Sprintf("Agent %q (%s)", name, agent.Binary)
			if _, err := exec.LookPath(agent.Binary); err != nil {
				printCheck(w, "FAIL", label, "not found on PATH")
				hasFail = true
			} else {
				printCheck(w, "OK", label, "found")
			}
		}
	}

	// 5. Web port availability (warn only).
	if cfg != nil && cfg.Web.Enabled != nil && *cfg.Web.Enabled {
		addr := net.JoinHostPort(cfg.Web.Host, strconv.Itoa(cfg.Web.Port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			printCheck(w, "WARN", "Web port", fmt.Sprintf("%s is not available (%v)", addr, err))
		} else {
			ln.Close()
			printCheck(w, "OK", "Web port", fmt.Sprintf("%s is available", addr))
		}
	}

	fmt.Fprintln(w)
	if hasFail {
		fmt.Fprintln(w, styleFail.Render("Some checks failed."))
		return fmt.Errorf("one or more checks failed")
	}
	fmt.Fprintln(w, styleOK.Render("All checks passed."))
	return nil
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
