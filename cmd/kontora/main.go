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
	for _, cmd := range []struct{ name, desc string }{
		{"ls", "List tickets (TUI on TTY, static table otherwise)"},
		{"new", "Create a ticket"},
		{"view", "Print ticket details"},
		{"edit", "Open a ticket in $EDITOR"},
		{"update", "Update ticket body/frontmatter fields"},
		{"delete", "Delete a ticket file"},
		{"init", "Set up a ticket for daemon processing"},
		{"run", "Enqueue a ticket for processing"},
		{"done", "Mark a ticket as done"},
		{"note", "Append a note to a ticket"},
		{"pause", "Pause a running or queued ticket"},
		{"retry", "Re-queue a paused ticket"},
		{"skip", "Skip to the next pipeline stage"},
		{"set-stage", "Move ticket to a specific pipeline stage"},
		{"cancel", "Cancel a ticket"},
		{"archive", "Archive old done/cancelled tickets"},
		{"logs", "Show agent logs for a ticket"},
		{"attach", "Attach to a running ticket"},
		{"start", "Start the daemon"},
		{"doctor", "Validate prerequisites and configuration"},
		{"config", "Show effective configuration"},
		{"fmt", "Format stream-json from stdin"},
		{"version", "Print version"},
		{"completion", "Generate shell completions"},
	} {
		fmt.Fprintf(&b, "  %-14s %s\n", helpCyan.Render(cmd.name), helpFaint.Render(cmd.desc))
	}
	return b.String()
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
	case "ls":
		cmdLs()
	case "new":
		cmdNew()
	case "view":
		cmdView()
	case "edit":
		cmdEdit()
	case "update":
		cmdUpdate()
	case "delete":
		cmdDelete()
	case "init":
		cmdInit()
	case "run":
		cmdRun()
	case "done":
		cmdDone()
	case "note":
		cmdNote()
	case "pause":
		cmdAction("pause")
	case "retry":
		cmdAction("retry")
	case "skip":
		cmdSkip()
	case "set-stage":
		cmdSetStage()
	case "cancel":
		cmdAction("cancel")
	case "archive":
		cmdArchive()
	case "logs":
		cmdLogs()
	case "attach":
		cmdAttach()
	case "start":
		cmdStart()
	case "doctor":
		cmdDoctor()
	case "config":
		cmdConfig()
	case "fmt":
		rejectInRemoteMode("fmt")
		if err := cli.Fmt(os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
	case "version":
		fmt.Printf("%s %s\n", helpBold.Render("kontora"), version)
	case "completion":
		cmdCompletion()

	default:
		fmt.Fprint(os.Stderr, renderUsage())
		os.Exit(1)
	}
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

	if *address != "" {
		cfg.Web.Host = *address
	}
	if *port != 0 {
		cfg.Web.Port = *port
	}
	cfg.ApplyServerEnvOverrides()

	if err := runDaemon(cfg, *configPath); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

func runDaemon(cfg *config.Config, configPath string) error {
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
	d := daemon.New(cfg, daemon.WithLogger(logger), daemon.WithLockPath(lockPath), daemon.WithConfigPath(configPath))
	return d.Run(ctx)
}

func cmdInit() {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	pipeline := fs.String("pipeline", "", "pipeline name (required in remote mode)")
	repoPath := fs.String("path", "", "repository path on the daemon host (required in remote mode)")
	agent := fs.String("agent", "", "agent override (optional)")
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

	if err := cli.Enable(cfg, taskID, os.Stdout); err != nil {
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
		printRemoteTickets(os.Stdout, tickets, running)
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
	pipeline := fs.String("pipeline", "", "pipeline name (optional)")
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
			Title:    title,
			Path:     *repoPath,
			Pipeline: *pipeline,
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
		Path:     path,
		Pipeline: *pipeline,
		Title:    title,
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
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora view TICKET_ID")
	}
	taskID := fs.Arg(0)

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
	pipeline := fs.String("pipeline", "", "set pipeline")
	repoPath := fs.String("path", "", "set repository path")
	agent := fs.String("agent", "", "set agent override (pass \"\" to clear)")
	branch := fs.String("branch", "", "set branch (pass \"\" to clear)")
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

	if req.Body == nil && req.Pipeline == nil && req.Path == nil && req.Agent == nil && req.Branch == nil {
		log.Fatal("nothing to update: pass at least one of --body-file, --pipeline, --path, --agent, --branch")
	}

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if err := rc.UpdateTicket(mustResolveRemote(rc, taskID), req); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Update(cfg, taskID, req); err != nil {
		log.Fatal(err)
	}
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
		if err := rc.DeleteTicket(mustResolveRemote(rc, taskID)); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Delete(cfg.TicketsDir, taskID); err != nil {
		log.Fatal(err)
	}
}

func cmdRun() {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora run TICKET_ID")
	}
	taskID := fs.Arg(0)

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if err := rc.Run(mustResolveRemote(rc, taskID)); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Run(cfg, taskID); err != nil {
		log.Fatal(err)
	}
}

func cmdDone() {
	fs := flag.NewFlagSet("done", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora done TICKET_ID")
	}
	taskID := fs.Arg(0)

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if err := rc.Done(mustResolveRemote(rc, taskID)); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.SetStatus(cfg.TicketsDir, taskID, "done"); err != nil {
		log.Fatal(err)
	}
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
		if err := rc.Note(mustResolveRemote(rc, taskID), text); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Note(cfg.TicketsDir, taskID, text); err != nil {
		log.Fatal(err)
	}
}

func cmdAction(action string) {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatalf("ticket ID is required: kontora %s TICKET_ID", action)
	}
	taskID := fs.Arg(0)

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
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	var err error
	switch action {
	case "pause":
		err = cli.Pause(cfg.TicketsDir, taskID)
	case "retry":
		err = cli.Retry(cfg.TicketsDir, taskID)
	case "cancel":
		err = cli.Cancel(cfg.TicketsDir, taskID)
	}
	if err != nil {
		log.Fatal(err)
	}
}

func cmdArchive() {
	rejectInRemoteMode("archive")

	fs := flag.NewFlagSet("archive", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	days := fs.Int("days", 0, "archive done/cancelled tickets not modified in the last N days")
	dryRun := fs.Bool("dry-run", false, "list tickets that would be archived without writing")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Archive(cfg, os.Stdout, cli.ArchiveOpts{Days: *days, DryRun: *dryRun}); err != nil {
		log.Fatal(err)
	}
}

func cmdSkip() {
	fs := flag.NewFlagSet("skip", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora skip TICKET_ID")
	}
	taskID := fs.Arg(0)

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if err := rc.Skip(mustResolveRemote(rc, taskID)); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.Skip(cfg, taskID); err != nil {
		log.Fatal(err)
	}
}

func cmdSetStage() {
	fs := flag.NewFlagSet("set-stage", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 2 {
		log.Fatal("usage: kontora set-stage TICKET_ID STAGE")
	}
	taskID := fs.Arg(0)
	stage := fs.Arg(1)

	if rc := remoteClient(*urlFlag, *tokenFlag); rc != nil {
		if err := rc.SetStage(mustResolveRemote(rc, taskID), stage); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg := mustLoadConfig(*configPath)

	if err := cli.SetStage(cfg, taskID, stage); err != nil {
		log.Fatal(err)
	}
}

func cmdLogs() {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	stage := fs.String("stage", "", "specific stage to show")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora logs [flags] TICKET_ID")
	}
	taskID := fs.Arg(0)

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
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	taskID := fs.Arg(0)

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
// config, opens it in $EDITOR, validates locally, and uploads it; changes apply
// only after the daemon restarts. In local mode it opens the on-disk config file
// in the editor directly.
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

	fmt.Println("Config saved. Restart the daemon for the changes to take effect.")
	return nil
}

func cmdCompletion() {
	rejectInRemoteMode("completion")

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

// parseTicketFlags parses a command whose single positional is the ticket ID,
// allowing flags to appear before and/or after the ID. Go's flag parser stops
// at the first positional, so we keep re-parsing the remaining args to pick up
// any flags written after the ID (e.g. `delete abc -f`). A second positional is
// rejected with a clear error instead of silently swallowing trailing flags.
// Returns the ID, or "" when none was given.
func parseTicketFlags(fs *flag.FlagSet, args []string) string {
	var id string
	for {
		if err := fs.Parse(args); err != nil {
			log.Fatalf("parsing flags: %v", err)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return id
		}
		if id != "" {
			log.Fatalf("unexpected argument %q", rest[0])
		}
		id = rest[0]
		args = rest[1:]
	}
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

func mustLoadConfig(configPath string) *config.Config {
	cfg, err := config.Load(configPath)
	if err == nil {
		return cfg
	}
	if errors.Is(err, config.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "\n  %s\n\n  Get started by running:\n\n    %s\n\n",
			helpFaint.Render("No configuration found."),
			helpCyan.Render("kontora start"),
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
	fi, statErr := os.Stdin.Stat()
	if statErr != nil || fi.Mode()&os.ModeCharDevice == 0 {
		log.Fatalf("config not found: %s\nRun \"kontora start\" to create one.", configPath)
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
