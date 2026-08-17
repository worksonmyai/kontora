package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/lipgloss"
	charmlog "github.com/charmbracelet/log"
	"github.com/mattn/go-isatty"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/cli/tui"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/daemon"
	"github.com/worksonmyai/kontora/internal/web"
)

func defaultConfigPath() string {
	return config.DefaultConfigPath()
}

var version = "dev"

var (
	helpBold  = lipgloss.NewStyle().Bold(true)
	helpFaint = lipgloss.NewStyle().Faint(true)
	helpCyan  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

func renderUsage() string {
	var b strings.Builder
	b.WriteString(helpBold.Render("Usage:") + " kontora <command>\n\n")
	b.WriteString(helpBold.Render("Commands:") + "\n")
	for _, cmd := range cli.Commands {
		fmt.Fprintf(&b, "  %-14s %s\n", helpCyan.Render(cmd.Name), helpFaint.Render(cmd.Desc))
	}
	return b.String()
}

// handlers maps each verb to what runs it. Every key must be a name in
// cli.Commands, which is what generates the usage text and the completions;
// TestDispatchCoversCommandTable holds the two together.
var handlers = map[string]func(){
	"ls":                  cmdLs,
	"new":                 cmdNew,
	"view":                cmdView,
	"edit":                cmdEdit,
	"update":              cmdUpdate,
	"delete":              cmdDelete,
	"init":                cmdInit,
	"run":                 cmdRun,
	"done":                func() { cmdAction("done") },
	"move":                cmdMove,
	"note":                cmdNote,
	"summary":             cmdSummary,
	"pause":               func() { cmdAction("pause") },
	"retry":               func() { cmdAction("retry") },
	"skip":                cmdSkip,
	"set-stage":           cmdSetStage,
	"cancel":              func() { cmdAction("cancel") },
	"archive":             cmdArchive,
	"logs":                cmdLogs,
	"activity":            cmdActivity,
	"changes":             cmdChanges,
	"stats":               cmdStats,
	"estimate-compaction": cmdEstimateCompaction,
	"review":              func() { cmdPlannotator("review") },
	"annotate":            func() { cmdPlannotator("annotate") },
	"attach":              cmdAttach,
	"start":               cmdStart,
	"setup":               cmdSetup,
	"doctor":              cmdDoctor,
	"config":              cmdConfig,
	"fmt":                 cmdFmt,
	"version":             cmdVersion,
	"completion":          cmdCompletion,
}

func main() {
	if len(os.Args) < 2 {
		if isatty.IsTerminal(os.Stdout.Fd()) {
			cmdLs()
		} else {
			fmt.Fprint(os.Stderr, renderUsage())
			os.Exit(1)
		}
		return
	}

	switch os.Args[1] {
	case "help", "-h", "--help":
		// An explicit request for help is not an error: it goes to stdout so it
		// can be piped, and exits 0 so a script does not read it as a failure.
		fmt.Print(renderUsage())
		return
	}

	run, ok := handlers[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, renderUsage())
		os.Exit(1)
	}
	run()
}

// cmdFmt and cmdCompletion read stdin and print text. They touch neither the
// daemon nor a config file, so an exported KONTORA_URL must not stop a shell rc
// from running "kontora completion fish | source".
func cmdFmt() {
	if err := cli.Fmt(os.Stdin, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdVersion() {
	fmt.Printf("%s %s\n", helpBold.Render("kontora"), version)
}

func cmdStart() {
	rejectInRemoteMode("start")

	fs := flag.NewFlagSet("start", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	address := fs.String("address", "", "web server listen address (overrides config)")
	port := fs.Int("port", 0, "web server port (overrides config)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	cfg := loadConfigOrSetup(*configPath)
	cfg.ApplyServerEnvOverrides()

	// The daemon applies this to the starting config and to every reload. The
	// flags never reach the config file, so a reload that only re-read the file
	// would drop them.
	override := func(c *config.Config) {
		if *address != "" {
			c.Web.Host = *address
		}
		if *port != 0 {
			c.Web.Port = *port
		}
	}

	if err := runDaemon(cfg, *configPath, override); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

func runDaemon(cfg *config.Config, configPath string, override func(*config.Config)) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop()
	}()

	lockPath := filepath.Join(filepath.Dir(configPath), "lock")

	logger := slog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
	}))
	d := daemon.New(cfg,
		daemon.WithLogger(logger),
		daemon.WithLockPath(lockPath),
		daemon.WithConfigPath(configPath),
		daemon.WithConfigOverride(override),
		daemon.WithVersion(version),
	)
	return d.Run(ctx)
}

// cmdSetup creates the config file. Plain `setup` runs the interactive wizard;
// `setup --agent` prints instructions for a coding agent instead and writes
// nothing. Both are local-only: the config they talk about is the one on this
// machine, not the remote daemon's.
func cmdSetup() {
	rejectInRemoteMode("setup")

	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	// Unlike every other command's --agent, this one is a boolean: setup has no
	// ticket to assign an agent to.
	agentMode := fs.Bool("agent", false, "print setup instructions for a coding agent instead of running the wizard")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	if fs.NArg() > 0 {
		log.Fatalf("unexpected argument %q: setup takes no positional arguments (--agent is a flag, not an agent name)", fs.Arg(0))
	}

	if *agentMode {
		if err := cli.WriteAgentSetupPrompt(*configPath, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	// The wizard reads keys and paints over its output stream, so without a
	// terminal on both ends it does neither usefully. An existing config is
	// reported without the wizard, so the guard only applies when there is
	// nothing on disk yet. A stat error that is not "no such file" says the path
	// itself is unusable; report it instead of walking into the wizard.
	switch _, err := os.Stat(*configPath); {
	case err == nil:
		// RunSetup reports the existing file and returns.
	case errors.Is(err, os.ErrNotExist):
		if !interactiveTerminal() {
			log.Fatalf("kontora setup needs an interactive terminal.\nRun \"kontora setup --agent\" to print setup instructions for a coding agent instead.")
		}
	default:
		log.Fatalf("setup: %v", err)
	}

	if err := cli.InitConfig(*configPath, os.Stdout); err != nil {
		if errors.Is(err, cli.ErrCancelled) {
			return
		}
		log.Fatalf("setup: %v", err)
	}
}

// interactiveTerminal reports whether both ends of the wizard's I/O are a
// terminal.
func interactiveTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

func cmdInit() {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	pipeline := fs.String("pipeline", "", "pipeline name, or \"none\" for a standalone ticket (required in remote mode)")
	repoPath := fs.String("path", "", "repository path, on the daemon host in remote mode (required in remote mode)")
	agent := fs.String("agent", "", "agent name, or \"none\" to skip the project default")
	stage := fs.String("stage", "", "starting pipeline stage (defaults to the pipeline's first stage)")
	status := fs.String("status", "", "initial status, open or todo (defaults to asking)")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])

	if taskID == "" {
		log.Fatal("ticket ID is required: kontora init TICKET_ID")
	}

	// Remote init is non-interactive: the TUI pickers don't work over HTTP, so
	// pipeline and path must be supplied as flags.
	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if *pipeline == "" || *repoPath == "" {
			log.Fatal("remote init requires --pipeline and --path")
		}
		if err := rc.Init(mustResolveRemote(rc, taskID), web.InitTicketRequest{
			Pipeline: *pipeline,
			Path:     *repoPath,
			Agent:    *agent,
		}); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	opts := cli.EnableOpts{Pipeline: *pipeline, Path: *repoPath, Agent: *agent, Stage: *stage, Status: *status}
	if err := cli.Enable(cfg, taskID, opts, os.Stdout); err != nil {
		if errors.Is(err, cli.ErrCancelled) {
			return
		}
		log.Fatal(err)
	}
}

func cmdLs() {
	var args []string
	if len(os.Args) >= 2 && os.Args[1] == "ls" {
		args = os.Args[2:]
	}

	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	closed := fs.Bool("closed", false, "show done/cancelled tickets")
	static := fs.Bool("static", false, "print static table instead of interactive TUI")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		tickets, running, err := rc.ListTickets()
		if err != nil {
			log.Fatal(err)
		}
		printRemoteTickets(os.Stdout, tickets, running, *closed)
		return
	}

	cfg := mustLoadConfig(*configPath)

	if !*static && !*closed && isatty.IsTerminal(os.Stdout.Fd()) {
		if err := tui.Run(cfg); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := cli.Status(cfg, os.Stdout, cli.StatusOpts{ShowClosed: *closed}); err != nil {
		log.Fatal(err)
	}
}

func cmdNew() {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	repoPath := fs.String("path", "", "repository path (defaults to current git root)")
	pipeline := fs.String("pipeline", "", "pipeline name, or \"none\" to skip the project default")
	agent := fs.String("agent", "", "agent name, or \"none\" to skip the project default")
	branch := fs.String("branch", "", "work branch name (defaults to <branch_prefix>/<id>)")
	baseBranch := fs.String("base-branch", "", "branch the work branch starts from (defaults to the repo default branch)")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	title := strings.Join(fs.Args(), " ")
	if title == "" {
		log.Fatal("title is required: kontora new [flags] TITLE")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		// Paths are on the daemon host, not the caller's machine, so a
		// local git-root default would be meaningless. Require --path.
		if *repoPath == "" {
			log.Fatal("remote new requires --path (a path on the daemon host)")
		}
		info, err := rc.CreateTicket(web.CreateTicketRequest{
			Title:      title,
			Path:       *repoPath,
			Pipeline:   *pipeline,
			Agent:      *agent,
			Branch:     *branch,
			BaseBranch: *baseBranch,
		})
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s %s\n", helpCyan.Render(info.ID), helpFaint.Render("created"))
		return
	}

	// Default to current git root if --path not specified.
	path := *repoPath
	if path == "" {
		var err error
		path, err = cli.GitRoot()
		if err != nil {
			log.Fatal(err)
		}
	}

	cfg := mustLoadConfig(*configPath)

	id, err := cli.Quick(cfg, cli.QuickOpts{
		Path:       path,
		Pipeline:   *pipeline,
		Agent:      *agent,
		Title:      title,
		Branch:     *branch,
		BaseBranch: *baseBranch,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s %s\n", helpCyan.Render(id), helpFaint.Render("created"))
}

func cmdView() {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatal("ticket ID is required: kontora view TICKET_ID")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		info, err := rc.GetTicket(mustResolveRemote(rc, taskID))
		if err != nil {
			log.Fatal(err)
		}
		printRemoteTicket(os.Stdout, info)
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.View(cfg, taskID, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdEdit() {
	rejectInRemoteMode("edit")

	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora edit TICKET_ID")
	}
	taskID := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)

	if err := cli.Edit(cfg.TicketsDir, cfg.Editor, taskID); err != nil {
		log.Fatal(err)
	}
}

func cmdUpdate() {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	bodyFile := fs.String("body-file", "", "read ticket body from a file ('-' for stdin)")
	pipeline := fs.String("pipeline", "", "set pipeline (pass \"\" or \"none\" to clear)")
	repoPath := fs.String("path", "", "set repository path")
	agent := fs.String("agent", "", "set agent override (pass \"\" or \"none\" to clear)")
	branch := fs.String("branch", "", "set branch (pass \"\" to clear)")
	baseBranch := fs.String("base-branch", "", "set base branch (pass \"\" to clear)")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])

	if taskID == "" {
		log.Fatal("ticket ID is required: kontora update TICKET_ID [flags]")
	}

	// Track which flags were actually passed so an explicit empty value (e.g.
	// --agent "") clears the field, distinct from a flag that was omitted.
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	var req web.UpdateTicketRequest
	if set["body-file"] {
		body, err := readBodyFile(*bodyFile)
		if err != nil {
			log.Fatal(err)
		}
		req.Body = &body
	}
	if set["pipeline"] {
		req.Pipeline = pipeline
	}
	if set["path"] {
		req.Path = repoPath
	}
	if set["agent"] {
		req.Agent = agent
	}
	if set["branch"] {
		req.Branch = branch
	}
	if set["base-branch"] {
		req.BaseBranch = baseBranch
	}

	if req.Body == nil && req.Pipeline == nil && req.Path == nil && req.Agent == nil && req.Branch == nil && req.BaseBranch == nil {
		log.Fatal("nothing to update: pass at least one of --body-file, --pipeline, --path, --agent, --branch, --base-branch")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.UpdateTicket(id, req); err != nil {
			log.Fatal(err)
		}
		confirm(id, "updated")
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Update(cfg, id, req); err != nil {
		log.Fatal(err)
	}
	confirm(id, "updated")
}

func readBodyFile(path string) (string, error) {
	if path == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading body from stdin: %w", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading body file: %w", err)
	}
	return string(data), nil
}

func cmdDelete() {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	force := fs.Bool("f", false, "confirm deletion (required)")
	yes := fs.Bool("yes", false, "confirm deletion (required)")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])

	if taskID == "" {
		log.Fatal("ticket ID is required: kontora delete TICKET_ID -f")
	}

	if !*force && !*yes {
		log.Fatal("refusing to delete without confirmation: pass -f or --yes")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.DeleteTicket(id); err != nil {
			log.Fatal(err)
		}
		confirm(id, "deleted")
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Delete(cfg.TicketsDir, id); err != nil {
		log.Fatal(err)
	}
	confirm(id, "deleted")
}

func cmdRun() {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatal("ticket ID is required: kontora run TICKET_ID")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.Run(id); err != nil {
			log.Fatal(err)
		}
		confirm(id, "queued")
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Run(cfg, id); err != nil {
		log.Fatal(err)
	}
	confirm(id, "queued")
}

func cmdNote() {
	fs := flag.NewFlagSet("note", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora note TICKET_ID [TEXT]")
	}
	taskID := fs.Arg(0)

	var text string
	if fs.NArg() >= 2 {
		text = strings.Join(fs.Args()[1:], " ")
	} else {
		fi, err := os.Stdin.Stat()
		if err != nil {
			log.Fatalf("stat stdin: %v", err)
		}
		if fi.Mode()&os.ModeCharDevice != 0 {
			log.Fatal("note text is required: kontora note TICKET_ID TEXT or echo TEXT | kontora note TICKET_ID")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("reading stdin: %v", err)
		}
		text = strings.TrimRight(string(data), "\n")
	}

	if text == "" {
		log.Fatal("note text is required (as argument or via stdin)")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.Note(id, text); err != nil {
			log.Fatal(err)
		}
		confirm(id, "note added")
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Note(cfg.TicketsDir, id, text); err != nil {
		log.Fatal(err)
	}
	confirm(id, "note added")
}

func cmdSummary() {
	fs := flag.NewFlagSet("summary", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora summary TICKET_ID [TEXT]")
	}
	taskID := fs.Arg(0)

	var text string
	if fs.NArg() >= 2 {
		text = strings.Join(fs.Args()[1:], " ")
	} else {
		fi, err := os.Stdin.Stat()
		if err != nil {
			log.Fatalf("stat stdin: %v", err)
		}
		if fi.Mode()&os.ModeCharDevice != 0 {
			log.Fatal("summary text is required: kontora summary TICKET_ID TEXT or echo TEXT | kontora summary TICKET_ID")
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			log.Fatalf("reading stdin: %v", err)
		}
		text = strings.TrimRight(string(data), "\n")
	}

	if text == "" {
		log.Fatal("summary text is required (as argument or via stdin)")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.Summary(id, text); err != nil {
			log.Fatal(err)
		}
		confirm(id, "summary set")
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Summary(cfg.TicketsDir, id, text); err != nil {
		log.Fatal(err)
	}
	confirm(id, "summary set")
}

func cmdAction(action string) {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatalf("ticket ID is required: kontora %s TICKET_ID", action)
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		var err error
		switch action {
		case "pause":
			err = rc.Pause(id)
		case "retry":
			err = rc.Retry(id)
		case "cancel":
			err = rc.Cancel(id)
		case "done":
			err = rc.Done(id)
		}
		if err != nil {
			log.Fatal(err)
		}
		confirm(id, actionPastTense[action])
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	var err error
	switch action {
	case "pause":
		err = cli.Pause(cfg, id)
	case "retry":
		err = cli.Retry(cfg.TicketsDir, id)
	case "cancel":
		err = cli.Cancel(cfg, id)
	case "done":
		err = cli.Done(cfg, id)
	}
	if err != nil {
		log.Fatal(err)
	}
	confirm(id, actionPastTense[action])
}

// actionPastTense names what each verb did, for the confirmation line.
var actionPastTense = map[string]string{
	"pause":  "paused",
	"retry":  "re-queued",
	"cancel": "cancelled",
	"done":   "done",
}

// daemonClient returns the client for whichever daemon the command targets: the
// remote one when a URL is set, else the local one this config describes. It is
// used by the verbs that only the daemon can answer — it owns the queue, the
// worktrees, and the run records.
func daemonClient(configPath, urlFlag, tokenFlag string) *remote.Client {
	if rc := remoteClient(urlFlag, tokenFlag); rc != nil {
		return rc
	}
	return cli.LocalClient(mustLoadConfig(configPath))
}

func cmdStats() {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	rng := fs.String("range", "90d", "window to report on: 1d, 1w, 30d, 90d or all")
	project := fs.String("project", "", "only count tickets for this configured project")
	pipeline := fs.String("pipeline", "", "only count tickets for this pipeline")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	info, err := daemonClient(*configPath, *urlFlag, *tokenFlag).Stats(*rng, *project, *pipeline)
	if err != nil {
		log.Fatal(err)
	}
	if err := cli.PrintStats(info, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdEstimateCompaction() {
	rejectInRemoteMode("estimate-compaction")

	fs := flag.NewFlagSet("estimate-compaction", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	logsDirFlag := fs.String("logs-dir", "", "path to logs directory (overrides config logs_dir)")
	stage := fs.String("stage", "implement", "pipeline stage to analyze")
	thresholdsStr := fs.String("thresholds", "100000,125000,150000,200000,250000", "comma-separated token thresholds")
	top := fs.Int("top", 20, "number of top sessions to show")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	thresholds, err := cli.ParseThresholds(*thresholdsStr)
	if err != nil {
		log.Fatalf("invalid --thresholds: %v", err)
	}

	logsDir := *logsDirFlag
	if logsDir == "" {
		cfg := mustLoadConfig(*configPath)
		logsDir = config.ExpandTilde(cfg.LogsDir)
	}

	report, err := cli.EstimateCompaction(logsDir, *stage, thresholds, *top)
	if err != nil {
		log.Fatal(err)
	}

	if err := cli.PrintCompactionEstimate(report, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdChanges() {
	fs := flag.NewFlagSet("changes", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatal("ticket ID is required: kontora changes TICKET_ID")
	}

	rc := daemonClient(*configPath, *urlFlag, *tokenFlag)
	info, err := rc.Changes(mustResolveRemote(rc, taskID))
	if err != nil {
		log.Fatal(err)
	}
	if err := cli.PrintChanges(info, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdActivity() {
	fs := flag.NewFlagSet("activity", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	stage := fs.String("stage", "", "stage to show (defaults to the most recent)")
	run := fs.Int("run", 0, "run number within the stage (defaults to the latest)")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatal("ticket ID is required: kontora activity TICKET_ID")
	}

	rc := daemonClient(*configPath, *urlFlag, *tokenFlag)
	info, err := rc.Activity(mustResolveRemote(rc, taskID), *stage, *run)
	if err != nil {
		log.Fatal(err)
	}
	if err := cli.PrintActivity(info, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// cmdPlannotator opens a ticket in Plannotator on the daemon host. The daemon
// spawns the binary, so a remote call opens it over there, not on the caller's
// screen.
func cmdPlannotator(action string) {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatalf("ticket ID is required: kontora %s TICKET_ID", action)
	}

	rc := daemonClient(*configPath, *urlFlag, *tokenFlag)
	id := mustResolveRemote(rc, taskID)

	var err error
	switch action {
	case "review":
		err = rc.PlannotatorReview(id)
	case "annotate":
		err = rc.PlannotatorAnnotate(id)
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s %s\n", helpCyan.Render(id), helpFaint.Render("opened in plannotator on the daemon host"))
}

// cmdMove is the general form of pause/cancel/done: it moves a ticket to any
// status the config allows, which is the only way to reach human_review or a
// custom status from the CLI.
func cmdMove() {
	fs := flag.NewFlagSet("move", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	args := parseArgs(fs, os.Args[2:], 2)
	if len(args) < 2 {
		log.Fatal("usage: kontora move TICKET_ID STATUS")
	}
	taskID, status := args[0], args[1]

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.Move(id, status); err != nil {
			log.Fatal(err)
		}
		confirm(id, "moved to "+status)
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Move(cfg, id, status); err != nil {
		log.Fatal(err)
	}
	confirm(id, "moved to "+status)
}

func cmdArchive() {
	rejectInRemoteMode("archive")

	fs := flag.NewFlagSet("archive", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	days := fs.Int("days", 0, "required: archive done/cancelled tickets not modified in the last N days")
	dryRun := fs.Bool("dry-run", false, "list tickets that would be archived without writing")
	repoPath := fs.String("path", "", "only archive tickets for this repository path")
	project := fs.String("project", "", "only archive tickets for this configured project")
	status := fs.String("status", "", "only archive tickets with this status (done or cancelled)")
	yes := fs.Bool("yes", false, "archive without asking for confirmation")
	yesShort := fs.Bool("y", false, "archive without asking for confirmation")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	cfg := mustLoadConfig(*configPath)

	// Only offer the prompt when stdin is a terminal. Piped input would answer
	// it by accident, so a non-interactive run has to pass --yes.
	var in io.Reader
	if isatty.IsTerminal(os.Stdin.Fd()) {
		in = os.Stdin
	}

	opts := cli.ArchiveOpts{Days: *days, DryRun: *dryRun, Path: *repoPath, Project: *project, Status: *status, Yes: *yes || *yesShort, In: in}
	if err := cli.Archive(cfg, os.Stdout, opts); err != nil {
		log.Fatal(err)
	}
}

func cmdSkip() {
	fs := flag.NewFlagSet("skip", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatal("ticket ID is required: kontora skip TICKET_ID")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.Skip(id); err != nil {
			log.Fatal(err)
		}
		confirm(id, "skipped to the next stage")
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.Skip(cfg, id); err != nil {
		log.Fatal(err)
	}
	confirm(id, "skipped to the next stage")
}

func cmdSetStage() {
	fs := flag.NewFlagSet("set-stage", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	args := parseArgs(fs, os.Args[2:], 2)
	if len(args) < 2 {
		log.Fatal("usage: kontora set-stage TICKET_ID STAGE")
	}
	taskID, stage := args[0], args[1]

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		id := mustResolveRemote(rc, taskID)
		if err := rc.SetStage(id, stage); err != nil {
			log.Fatal(err)
		}
		confirm(id, "stage set to "+stage)
		return
	}

	cfg := mustLoadConfig(*configPath)
	id := mustResolveLocal(cfg, taskID)

	if err := cli.SetStage(cfg, id, stage); err != nil {
		log.Fatal(err)
	}
	confirm(id, "stage set to "+stage)
}

func cmdLogs() {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	stage := fs.String("stage", "", "specific stage to show")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])
	if taskID == "" {
		log.Fatal("ticket ID is required: kontora logs [flags] TICKET_ID")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		content, err := rc.Logs(mustResolveRemote(rc, taskID), *stage)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprint(os.Stdout, content)
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Logs(cfg.TicketsDir, cfg.LogsDir, taskID, *stage, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdAttach() {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	rw := fs.Bool("rw", false, "attach in read-write mode")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	taskID := parseTicketFlags(fs, os.Args[2:])

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if taskID == "" {
			log.Fatal("ticket ID is required: kontora attach TICKET_ID")
		}
		if err := remote.Attach(context.Background(), rc, mustResolveRemote(rc, taskID), *rw); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Attach(cfg, taskID, *rw); err != nil {
		if errors.Is(err, cli.ErrCancelled) {
			return
		}
		log.Fatal(err)
	}
}

func cmdConfig() {
	if len(os.Args) >= 3 && os.Args[2] == "edit" {
		cmdConfigEdit()
		return
	}

	fs := flag.NewFlagSet("config", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		cfg, err := rc.Config()
		if err != nil {
			log.Fatal(err)
		}
		printRemoteConfig(os.Stdout, cfg)
		return
	}

	cfg := mustLoadConfig(*configPath)
	if err := cli.ShowConfig(cfg, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// cmdConfigEdit edits the daemon's config. In remote mode it fetches the raw
// config, opens it in $EDITOR, validates locally, and uploads it; the daemon
// reloads as part of the save. In local mode it opens the on-disk config file
// in the editor directly, and the daemon's config watcher picks up the save.
func cmdConfigEdit() {
	fs := flag.NewFlagSet("config edit", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[3:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if err := remoteConfigEdit(rc); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Local: open the on-disk config file directly. Use the configured editor
	// when the config loads, otherwise fall back to $EDITOR/vi so a broken
	// config can still be repaired.
	editor := ""
	if cfg, err := config.Load(*configPath); err == nil {
		editor = cfg.Editor
	}
	if err := cli.EditFile(editor, *configPath); err != nil {
		log.Fatal(err)
	}
}

func remoteConfigEdit(rc *remote.Client) error {
	content, err := rc.RawConfig()
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "kontora-config-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := cli.EditFile("", tmpPath); err != nil {
		return err
	}

	edited, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}

	// Validate locally first for a fast, clear error before the round-trip.
	if _, err := config.LoadReader(strings.NewReader(string(edited))); err != nil {
		return fmt.Errorf("edited config is invalid, not saving: %w", err)
	}

	if err := rc.PutRawConfig(string(edited)); err != nil {
		return err
	}

	// A relative docs path is no use to a caller on another machine, so name the
	// published page instead.
	fmt.Println("Config saved and reloaded. Settings that need a restart are listed at https://worksonmyai.github.io/kontora/configuration/")
	return nil
}

func cmdCompletion() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "%s kontora completion <shell>\n\n%s fish\n", helpBold.Render("Usage:"), helpFaint.Render("Supported shells:"))
		os.Exit(1)
	}
	if err := cli.Completion(os.Args[2], os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdDoctor() {
	rejectInRemoteMode("doctor")

	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	if err := cli.Doctor(*configPath, os.Stdout); err != nil {
		os.Exit(1)
	}
}

// parseArgs parses a command's flags and up to max positionals, allowing flags
// to appear before, between, and after the positionals. Go's flag parser stops
// at the first positional, so we keep re-parsing the remainder to pick up any
// flag written after one (e.g. `delete abc -f`). A positional past max is
// rejected with a clear error instead of silently swallowing a trailing flag.
//
// Commands whose last positional is free-form text (note, summary, new) do not
// use this: a flag-looking word inside the text belongs to the text.
func parseArgs(fs *flag.FlagSet, args []string, maxPositional int) []string {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			log.Fatalf("parsing flags: %v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		if len(positional) >= maxPositional {
			log.Fatalf("unexpected argument %q", rest[0])
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// parseTicketFlags is parseArgs for the common case of one ticket ID. It
// returns "" when none was given.
func parseTicketFlags(fs *flag.FlagSet, args []string) string {
	if got := parseArgs(fs, args, 1); len(got) > 0 {
		return got[0]
	}
	return ""
}

// addRemoteFlags registers --url and --token on a command's flag set, with
// KONTORA_URL/KONTORA_TOKEN as defaults. A non-empty resolved URL switches the
// command into remote mode.
func addRemoteFlags(fs *flag.FlagSet) (url, token *string) {
	url = fs.String("url", os.Getenv("KONTORA_URL"), "remote daemon URL (or KONTORA_URL); enables remote mode")
	token = fs.String("token", os.Getenv("KONTORA_TOKEN"), "bearer token for the remote daemon (or KONTORA_TOKEN)")
	return url, token
}

// remoteClient returns a remote.Client when a URL is configured, else nil
// (local mode).
func remoteClient(url, token string) *remote.Client {
	if url == "" {
		return nil
	}
	return remote.New(url, token)
}

// remoteModeRequested reports whether remote mode is active via the env var.
// Used by local-only verbs that do not parse remote flags.
func remoteModeRequested() bool {
	return os.Getenv("KONTORA_URL") != ""
}

// rejectInRemoteMode aborts a local-only verb when remote mode is requested.
func rejectInRemoteMode(verb string) {
	if remoteModeRequested() {
		log.Fatalf("%q is not available in remote mode", verb)
	}
}

// mustResolveRemote expands a ticket ID prefix against the remote daemon.
func mustResolveRemote(rc *remote.Client, taskID string) string {
	id, err := rc.ResolveID(taskID)
	if err != nil {
		log.Fatal(err)
	}
	return id
}

// mustResolveLocal expands a ticket ID prefix against the local tickets dir.
func mustResolveLocal(cfg *config.Config, taskID string) string {
	id, err := cli.ResolveTicketID(config.ExpandTilde(cfg.TicketsDir), taskID)
	if err != nil {
		log.Fatal(err)
	}
	return id
}

// confirm prints the acknowledgement a mutating verb ends with. Every one of
// them prints the resolved ID, so a run that passed a prefix can be read back
// to the ticket it actually changed.
func confirm(id, what string) {
	fmt.Printf("%s %s\n", helpCyan.Render(id), helpFaint.Render(what))
}

func mustLoadConfig(configPath string) *config.Config {
	cfg, err := config.Load(configPath)
	if err == nil {
		return cfg
	}
	if errors.Is(err, config.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "\n  %s\n\n  Create one interactively:\n\n    %s\n\n  Or print instructions for a coding agent:\n\n    %s\n\n",
			helpFaint.Render("No configuration found at "+configPath),
			helpCyan.Render("kontora setup"),
			helpCyan.Render("kontora setup --agent"),
		)
		os.Exit(1)
	}
	log.Fatalf("loading config: %v", err)
	return nil
}

func loadConfigOrSetup(configPath string) *config.Config {
	cfg, err := config.Load(configPath)
	if err == nil {
		return cfg
	}
	if !errors.Is(err, config.ErrNotFound) {
		log.Fatalf("loading config: %v", err)
	}
	if !interactiveTerminal() {
		log.Fatalf("config not found: %s\nRun \"kontora setup\" on a terminal, or \"kontora setup --agent\" to print setup instructions for a coding agent.", configPath)
	}
	fmt.Fprintf(os.Stderr, "No config found. Running setup...\n\n")
	if setupErr := cli.InitConfig(configPath, os.Stdout); setupErr != nil {
		if errors.Is(setupErr, cli.ErrCancelled) {
			os.Exit(0)
		}
		log.Fatalf("setup: %v", setupErr)
	}
	cfg, err = config.Load(configPath)
	if err != nil {
		log.Fatalf("loading config after setup: %v", err)
	}
	return cfg
}
