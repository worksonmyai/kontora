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
	"strings"
	"syscall"

	"github.com/charmbracelet/lipgloss"
	charmlog "github.com/charmbracelet/log"
	"github.com/mattn/go-isatty"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/tui"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/daemon"
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
		{"init", "Set up a ticket for daemon processing"},
		{"done", "Mark a ticket as done"},
		{"note", "Append a note to a ticket"},
		{"pause", "Pause a running or queued ticket"},
		{"retry", "Re-queue a paused ticket"},
		{"skip", "Skip to the next pipeline stage"},
		{"set-stage", "Move ticket to a specific pipeline stage"},
		{"cancel", "Cancel a ticket"},
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
	case "init":
		cmdInit()
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

	if err := runDaemon(cfg); err != nil {
		log.Fatalf("daemon: %v", err)
	}
}

func runDaemon(cfg *config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop()
	}()

	logger := slog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
	}))
	d := daemon.New(cfg, daemon.WithLogger(logger))
	return d.Run(ctx)
}

func cmdInit() {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora init TICKET_ID")
	}
	taskID := fs.Arg(0)

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
	all := fs.Bool("all", false, "show all tickets including non-initialized")
	closed := fs.Bool("closed", false, "show done/cancelled tickets")
	static := fs.Bool("static", false, "print static table instead of interactive TUI")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	cfg := mustLoadConfig(*configPath)

	if !*static && !*all && !*closed && isatty.IsTerminal(os.Stdout.Fd()) {
		if err := tui.Run(cfg); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := cli.Status(cfg, *all, os.Stdout, cli.StatusOpts{ShowClosed: *closed}); err != nil {
		log.Fatal(err)
	}
}

func cmdNew() {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	repoPath := fs.String("path", "", "repository path (defaults to current git root)")
	pipeline := fs.String("pipeline", "", "pipeline name (optional)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	title := strings.Join(fs.Args(), " ")
	if title == "" {
		log.Fatal("title is required: kontora new [flags] TITLE")
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
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora view TICKET_ID")
	}
	taskID := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)

	if err := cli.View(cfg, taskID, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdEdit() {
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

func cmdDone() {
	fs := flag.NewFlagSet("done", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora done TICKET_ID")
	}
	taskID := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)

	if err := cli.SetStatus(cfg.TicketsDir, taskID, "done"); err != nil {
		log.Fatal(err)
	}
}

func cmdNote() {
	fs := flag.NewFlagSet("note", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
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

	cfg := mustLoadConfig(*configPath)

	if err := cli.Note(cfg.TicketsDir, taskID, text); err != nil {
		log.Fatal(err)
	}
}

func cmdAction(action string) {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatalf("ticket ID is required: kontora %s TICKET_ID", action)
	}
	taskID := fs.Arg(0)

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

func cmdSkip() {
	fs := flag.NewFlagSet("skip", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora skip TICKET_ID")
	}
	taskID := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)

	if err := cli.Skip(cfg, taskID); err != nil {
		log.Fatal(err)
	}
}

func cmdSetStage() {
	fs := flag.NewFlagSet("set-stage", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 2 {
		log.Fatal("usage: kontora set-stage TICKET_ID STAGE")
	}
	taskID := fs.Arg(0)
	stage := fs.Arg(1)

	cfg := mustLoadConfig(*configPath)

	if err := cli.SetStage(cfg, taskID, stage); err != nil {
		log.Fatal(err)
	}
}

func cmdLogs() {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	stage := fs.String("stage", "", "specific stage to show")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	if fs.NArg() < 1 {
		log.Fatal("ticket ID is required: kontora logs [flags] TICKET_ID")
	}
	taskID := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)

	if err := cli.Logs(cfg.TicketsDir, cfg.LogsDir, taskID, *stage, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func cmdAttach() {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	rw := fs.Bool("rw", false, "attach in read-write mode")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}

	taskID := fs.Arg(0)

	cfg := mustLoadConfig(*configPath)

	if err := cli.Attach(cfg, taskID, *rw); err != nil {
		if errors.Is(err, cli.ErrCancelled) {
			return
		}
		log.Fatal(err)
	}
}

func cmdConfig() {
	cfg := loadConfig(os.Args[2:])
	if err := cli.ShowConfig(cfg, os.Stdout); err != nil {
		log.Fatal(err)
	}
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
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	if err := cli.Doctor(*configPath, os.Stdout); err != nil {
		os.Exit(1)
	}
}

func loadConfig(args []string) *config.Config {
	fs := flag.NewFlagSet("", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parsing flags: %v", err)
	}
	return mustLoadConfig(*configPath)
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
