package cli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

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

	// The config has to parse before any check that reads it.
	cfg, err := config.Load(configPath)
	var ticketsDirEnv, ticketsDirFile string
	if err != nil {
		check("FAIL", "Config", err.Error())
		cfg = nil
	} else {
		check("OK", "Config", configPath)
		// The environment outranks the file for tickets_dir, so record what the
		// file said before folding it in: this is the one setting that can move
		// under the user without the config showing it.
		ticketsDirFile = config.ExpandTilde(cfg.TicketsDir)
		ticketsDirEnv = cfg.ApplyEnvOverrides()
	}

	daemonRunning := checkDaemon(configPath, check)

	checkDirs(cfg, ticketsDirEnv, ticketsDirFile, check)
	checkTools(check)
	checkAgents(cfg, check)
	checkProjects(cfg, check)
	checkPlannotator(cfg, check)
	checkWebPort(cfg, daemonRunning, check)

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

// checkFunc records one check result under a level of "OK", "WARN" or "FAIL".
type checkFunc func(level, name, detail string)

// checkDaemon reports whether a daemon holds the lock beside the config, and
// returns that answer for the checks that read it. A stopped daemon is the
// usual reason a new ticket sits at todo and nothing happens, so it warns.
func checkDaemon(configPath string, check checkFunc) bool {
	running, err := daemonHoldsLock(filepath.Join(filepath.Dir(configPath), "lock"))
	switch {
	case err != nil:
		check("WARN", "Daemon", fmt.Sprintf("could not read the lock file: %v", err))
		return false
	case running:
		check("OK", "Daemon", "running")
		return true
	default:
		check("WARN", "Daemon", "not running. Run `kontora start` to pick up tickets.")
		return false
	}
}

// daemonHoldsLock reports whether another process holds the daemon's flock on
// lockPath. A daemon removes the file on exit, so a missing file means no
// daemon; a file nothing locks is left over from a kill.
//
// It never removes the file, because it runs beside a live daemon. A daemon
// that has created its lock file but not yet taken the flock reads as stale in
// that instant.
func daemonHoldsLock(lockPath string) (bool, error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// checkDirs reports the three working directories. They are warn-only: the
// daemon creates them on first use.
func checkDirs(cfg *config.Config, ticketsDirEnv, ticketsDirFile string, check checkFunc) {
	if cfg == nil {
		return
	}
	ticketsDetail := ""
	if ticketsDirEnv != "" {
		ticketsDetail = fmt.Sprintf(" (from $%s, overriding config)", ticketsDirEnv)
	}
	for _, dir := range []struct {
		name   string
		path   string
		detail string
	}{
		{"Tickets dir", config.ExpandTilde(cfg.TicketsDir), ticketsDetail},
		{"Logs dir", config.ExpandTilde(cfg.LogsDir), ""},
		{"Worktrees dir", config.ExpandTilde(cfg.WorktreesDir), ""},
	} {
		if info, err := os.Stat(dir.path); err == nil && info.IsDir() {
			check("OK", dir.name, dir.path+dir.detail)
		} else {
			check("WARN", dir.name, fmt.Sprintf("%s%s (will be auto-created)", dir.path, dir.detail))
		}
	}
	if ticketsDirEnv != "" && ticketsDirFile != config.ExpandTilde(cfg.TicketsDir) {
		check("WARN", "Tickets dir conflict", fmt.Sprintf(
			"$%s points at %s, but the config says %s. Every command reads the first; a daemon that cannot see the variable reads the second.",
			ticketsDirEnv, config.ExpandTilde(cfg.TicketsDir), ticketsDirFile))
	}
}

// checkTools reports the binaries kontora cannot run without.
func checkTools(check checkFunc) {
	for _, tool := range []string{"git", "tmux"} {
		if _, err := exec.LookPath(tool); err != nil {
			check("FAIL", tool, "not found on PATH")
		} else {
			check("OK", tool, "found")
		}
	}
}

// checkAgents reports each configured agent binary.
func checkAgents(cfg *config.Config, check checkFunc) {
	if cfg == nil {
		return
	}
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

// checkProjects reports each project repository. A path that is not a git
// worktree cannot have a branch cut from it, so every ticket for that project
// pauses at pickup. With nothing to select a pipeline from, a new ticket also
// runs no pipeline at all.
func checkProjects(cfg *config.Config, check checkFunc) {
	if cfg == nil {
		return
	}
	if noPipelineDefault(cfg) {
		check("WARN", "Pipeline defaults", "no default_pipeline, and no project sets one, so a new ticket runs one agent on its description rather than a pipeline")
	}
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

// noPipelineDefault reports whether nothing in the config can give a new ticket
// a pipeline. A project that sets one still leaves paths outside it bare, but
// the user has said what they want, so it stays quiet.
func noPipelineDefault(cfg *config.Config) bool {
	if cfg.DefaultPipeline != "" {
		return false
	}
	for _, project := range cfg.Projects {
		if project.Pipeline != "" {
			return false
		}
	}
	return true
}

// checkPlannotator is warn-only: plannotator is needed for `review` and
// `annotate`, not for running agents.
func checkPlannotator(cfg *config.Config, check checkFunc) {
	if cfg == nil || cfg.Plannotator.Binary == "" {
		return
	}
	label := fmt.Sprintf("Plannotator (%s)", cfg.Plannotator.Binary)
	if resolved, err := process.LookupBinary(cfg.Plannotator.Binary); err != nil {
		check("WARN", label, "not found on PATH (only needed for review/annotate)")
	} else {
		check("OK", label, resolved)
	}
}

// checkWebPort is warn-only: a port the dashboard answers on is its own, not a
// conflict. A running daemon is not proof of that on its own, because the
// daemon carries on when its web server cannot listen, so the address is asked
// directly.
func checkWebPort(cfg *config.Config, daemonRunning bool, check checkFunc) {
	if cfg == nil || cfg.Web.Enabled == nil || !*cfg.Web.Enabled {
		return
	}
	addr := net.JoinHostPort(cfg.Web.Host, strconv.Itoa(cfg.Web.Port))
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		check("OK", "Web port", fmt.Sprintf("%s is available", addr))
		return
	}
	if servesDashboard(addr) {
		check("OK", "Web port", fmt.Sprintf("%s is serving the kontora dashboard", addr))
		return
	}
	detail := fmt.Sprintf("%s is taken by another process (%v)", addr, err)
	if daemonRunning {
		detail += ", so the running daemon has no dashboard"
	}
	check("WARN", "Web port", detail)
}

// servesDashboard reports whether whatever holds addr answers the dashboard's
// /health with a 200. That endpoint takes no token, so the probe also works
// against a daemon configured with one.
func servesDashboard(addr string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
