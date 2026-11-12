package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/remote"
)

type automationFlags struct {
	spec                         automation.Spec
	key, promptFile, requestID   string
	state, query, project, agent string
	revision, run                uint64
	offset, limit                int
	json, raw, yes, force, rw    bool
	set                          map[string]bool
}

func automationCommand(name string) (cli.Command, bool) {
	for _, command := range cli.Commands {
		if command.Name != "automation" {
			continue
		}
		if name == "" {
			return command, true
		}
		for _, child := range command.Subcommands {
			if child.Name == name {
				return child, true
			}
		}
	}
	return cli.Command{}, false
}

func cmdAutomation() {
	if len(os.Args) < 3 || os.Args[2] == "-h" || os.Args[2] == "--help" {
		command, _ := automationCommand("")
		fmt.Fprintln(os.Stdout, "Usage: kontora automation COMMAND [flags]\n\nManage automations through the daemon API.\n\nCommands:")
		for _, child := range command.Subcommands {
			fmt.Fprintf(os.Stdout, "  %-12s %s\n", child.Name, child.Desc)
		}
		return
	}
	sub := os.Args[2]
	command, ok := automationCommand(sub)
	if !ok {
		log.Fatalf("unknown automation command %q; use kontora automation -h", sub)
	}
	fs := flag.NewFlagSet("automation "+sub, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: kontora automation %s %s [flags]\n\n%s\n\n", sub, command.Args, command.Desc)
		fs.PrintDefaults()
	}
	configPath := fs.String("config", defaultConfigPath(), "path to config file")
	urlFlag, tokenFlag := addRemoteFlags(fs)
	operator := fs.String("operator-token", os.Getenv("KONTORA_OPERATOR_TOKEN"), "operator credential (or KONTORA_OPERATOR_TOKEN)")
	opts := automationFlags{set: make(map[string]bool)}
	opts.addFlags(fs, sub)
	if sub == "update" {
		for _, field := range command.Flags {
			if f := fs.Lookup(field.Name); f != nil {
				f.Usage = field.Desc
			}
		}
	}
	maxArgs := 1
	if sub == "new" || sub == "ls" || sub == "preview" {
		maxArgs = 0
	}
	pos := parseArgs(fs, os.Args[3:], maxArgs)
	fs.Visit(func(f *flag.Flag) { opts.set[f.Name] = true })
	ref := ""
	if maxArgs == 1 {
		if len(pos) == 0 {
			log.Fatal("automation reference is required")
		}
		ref = pos[0]
	}
	if err := opts.validate(sub, ref); err != nil {
		log.Fatal(err)
	}
	if opts.set["prompt-file"] {
		body, err := readBodyFile(opts.promptFile)
		if err != nil {
			log.Fatal(err)
		}
		opts.spec.Prompt = body
	}
	rc := daemonClient(*configPath, *urlFlag, *tokenFlag).WithOperatorToken(*operator)
	if err := executeAutomation(rc, sub, ref, opts, os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

func (opts *automationFlags) addFlags(fs *flag.FlagSet, sub string) {
	if sub != "attach" {
		fs.BoolVar(&opts.json, "json", false, "print JSON")
	}
	if sub == "new" || sub == "update" {
		defaults := automation.Spec{}
		if sub == "new" {
			defaults = automation.Spec{Timezone: "UTC", Workspace: "worktree", Overlap: "skip", Timeout: "30m"}
		}
		fs.StringVar(&opts.spec.Name, "name", "", "automation name")
		fs.StringVar(&opts.promptFile, "prompt-file", "", "prompt template file ('-' for stdin)")
		fs.StringVar(&opts.spec.Path, "path", "", "repository path on the daemon host")
		fs.StringVar(&opts.spec.Agent, "agent", "", "configured agent")
		fs.StringVar(&opts.spec.Model, "model", "", "model override (empty clears it)")
		fs.StringVar(&opts.spec.Schedule, "schedule", "", "five-field cron or interval")
		fs.StringVar(&opts.spec.Timezone, "timezone", defaults.Timezone, "IANA timezone")
		fs.StringVar(&opts.spec.Workspace, "workspace", defaults.Workspace, "worktree or repo")
		fs.StringVar(&opts.spec.Overlap, "overlap", defaults.Overlap, "skip, queue or replace")
		fs.StringVar(&opts.spec.Timeout, "timeout", defaults.Timeout, "positive run timeout")
	}
	if sub == "new" || sub == "duplicate" {
		fs.StringVar(&opts.key, "key", "", "configuration key (derived from name when empty)")
	}
	if sub == "duplicate" {
		fs.StringVar(&opts.spec.Name, "name", "", "name for the copy")
	}
	if sub == "new" || sub == "update" || sub == "duplicate" {
		fs.BoolVar(&opts.spec.Enabled, "enabled", false, "enable scheduled runs")
	}
	if sub == "new" || sub == "duplicate" || sub == "run" {
		fs.StringVar(&opts.requestID, "request-id", "", "retry identity (generated when omitted)")
	}
	if sub == "update" || sub == "delete" || sub == "pause" || sub == "resume" {
		fs.Uint64Var(&opts.revision, "revision", 0, "expected revision (defaults to fetched revision)")
	}
	if sub == "stop" || sub == "logs" || sub == "terminal" || sub == "attach" {
		fs.Uint64Var(&opts.run, "run", 0, "exact run number")
	}
	switch sub {
	case "ls":
		fs.StringVar(&opts.state, "state", "all", "all, enabled or paused")
		fs.StringVar(&opts.query, "query", "", "case-insensitive substring")
		fs.StringVar(&opts.project, "project", "", "exact project")
		fs.StringVar(&opts.agent, "agent", "", "exact agent")
	case "delete":
		fs.BoolVar(&opts.yes, "yes", false, "confirm deletion (required)")
		fs.BoolVar(&opts.force, "f", false, "confirm deletion (required)")
	case "runs":
		fs.IntVar(&opts.offset, "offset", 0, "history offset")
		fs.IntVar(&opts.limit, "limit", 20, "page size (1 through 200)")
	case "logs":
		fs.BoolVar(&opts.raw, "raw", false, "print composed log text unchanged")
	case "preview":
		fs.StringVar(&opts.spec.Schedule, "schedule", "", "five-field cron or interval (required)")
		fs.StringVar(&opts.spec.Timezone, "timezone", "UTC", "IANA timezone")
	case "attach":
		fs.BoolVar(&opts.rw, "rw", false, "attach in read-write mode")
	}
}

func (opts automationFlags) validate(sub, ref string) error {
	if opts.set["revision"] && opts.revision == 0 {
		return errors.New("--revision must be positive")
	}
	if (sub == "stop" || sub == "logs" || sub == "terminal" || opts.set["run"]) && opts.run == 0 {
		return errors.New("--run must be a positive run number")
	}
	if sub == "delete" && !opts.yes && !opts.force {
		return errors.New("deletion requires --yes or -f")
	}
	if sub == "logs" && opts.raw && opts.json {
		return errors.New("--raw and --json are mutually exclusive")
	}
	if sub == "runs" && (opts.offset < 0 || opts.limit < 1 || opts.limit > 200) {
		return errors.New("--offset must be nonnegative and --limit must be from 1 through 200")
	}
	if sub == "ls" && opts.state != "all" && opts.state != "enabled" && opts.state != "paused" {
		return errors.New("--state must be all, enabled or paused")
	}
	return opts.validateMutation(sub, ref)
}

func (opts automationFlags) validateMutation(sub, ref string) error {
	if opts.set["request-id"] && (strings.TrimSpace(opts.requestID) == "" || len(opts.requestID) > 200) {
		return errors.New("--request-id must contain 1 through 200 bytes and not be blank")
	}
	if sub == "run" && opts.set["request-id"] && !automation.IsID(ref) {
		return errors.New("--request-id requires the original full automation ID, not a configuration key")
	}
	if sub == "new" && opts.promptFile == "" {
		return errors.New("--prompt-file is required")
	}
	if sub == "update" && !opts.hasChanges() {
		return errors.New("update requires at least one specification field")
	}
	if sub == "preview" && opts.spec.Schedule == "" {
		return errors.New("--schedule is required")
	}
	return nil
}

func (opts automationFlags) hasChanges() bool {
	for _, name := range []string{"name", "prompt-file", "path", "agent", "model", "schedule", "timezone", "workspace", "overlap", "timeout", "enabled"} {
		if opts.set[name] {
			return true
		}
	}
	return false
}

func (opts automationFlags) changes() cli.AutomationUpdate {
	var changes cli.AutomationUpdate
	for _, field := range []struct {
		name   string
		value  *string
		target **string
	}{
		{"name", &opts.spec.Name, &changes.Name}, {"prompt-file", &opts.spec.Prompt, &changes.Prompt},
		{"path", &opts.spec.Path, &changes.Path}, {"agent", &opts.spec.Agent, &changes.Agent},
		{"model", &opts.spec.Model, &changes.Model}, {"schedule", &opts.spec.Schedule, &changes.Schedule},
		{"timezone", &opts.spec.Timezone, &changes.Timezone}, {"workspace", &opts.spec.Workspace, &changes.Workspace},
		{"overlap", &opts.spec.Overlap, &changes.Overlap}, {"timeout", &opts.spec.Timeout, &changes.Timeout},
	} {
		if opts.set[field.name] {
			*field.target = field.value
		}
	}
	if opts.set["enabled"] {
		changes.Enabled = &opts.spec.Enabled
	}
	return changes
}

func executeAutomation(c *remote.Client, sub, ref string, opts automationFlags, out, diagnostics io.Writer) error {
	definition := func(row automation.Definition, err error) error {
		if err != nil {
			return err
		}
		if opts.json {
			return json.NewEncoder(out).Encode(row)
		}
		return cli.WriteAutomation(out, row)
	}
	switch sub {
	case "ls":
		return writeAutomationList(c, opts, out)
	case "preview":
		return writeAutomationPreview(c, opts, out)
	case "new":
		return definition(cli.NewAutomation(c, opts.key, opts.requestID, opts.spec, diagnostics))
	case "run":
		row, err := cli.RunAutomationRequest(c, ref, opts.requestID, diagnostics)
		if err != nil {
			return err
		}
		if opts.json {
			return json.NewEncoder(out).Encode(row)
		}
		return cli.WriteAutomationRun(out, row)
	}
	id, err := cli.ResolveAutomation(c, ref)
	if err != nil {
		return err
	}
	return executeAutomationTarget(c, sub, id, opts, out, diagnostics)
}

func executeAutomationTarget(c *remote.Client, sub, id string, opts automationFlags, out, diagnostics io.Writer) error {
	encode := func(v any) error { return json.NewEncoder(out).Encode(v) }
	definition := func(row automation.Definition, err error) error {
		if err != nil {
			return err
		}
		if opts.json {
			return encode(row)
		}
		return cli.WriteAutomation(out, row)
	}
	switch sub {
	case "view":
		return definition(c.Automation(id))
	case "update":
		return definition(cli.UpdateAutomation(c, id, opts.revision, opts.changes()))
	case "duplicate":
		current, err := c.Automation(id)
		if err != nil {
			return err
		}
		spec := current.Spec
		spec.Name += " (copy)"
		if opts.set["name"] {
			spec.Name = opts.spec.Name
		}
		spec.Enabled = opts.spec.Enabled
		return definition(cli.NewAutomation(c, opts.key, opts.requestID, spec, diagnostics))
	case "pause":
		return definition(cli.PauseAutomation(c, id, opts.revision))
	case "resume":
		return definition(cli.ResumeAutomation(c, id, opts.revision))
	case "delete":
		if err := cli.DeleteAutomation(c, id, opts.revision); err != nil {
			return err
		}
		if opts.json {
			return encode(struct {
				ID      string `json:"id"`
				Deleted bool   `json:"deleted"`
			}{id, true})
		}
		_, err := fmt.Fprintf(out, "deleted %s\n", id)
		return err
	case "stop":
		row, err := c.StopAutomation(id, opts.run)
		if err != nil {
			return err
		}
		if opts.json {
			return encode(row)
		}
		return cli.WriteAutomationRun(out, row)
	case "runs":
		response, err := c.AutomationRuns(id, opts.offset, opts.limit)
		if err != nil {
			return err
		}
		if opts.json {
			return encode(response)
		}
		if err := cli.WriteAutomationRuns(out, response.Runs); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Total: %d (offset %d, limit %d)\n", response.Total, opts.offset, opts.limit)
		return err
	case "logs":
		return writeAutomationLog(c, id, opts, out)
	case "terminal":
		response, err := c.AutomationTerminal(id, opts.run)
		if err != nil {
			return err
		}
		if opts.json {
			return encode(response)
		}
		return writeAutomationTerminal(out, response)
	case "attach":
		return remote.AttachAutomation(context.Background(), c, id, opts.run, opts.rw)
	}
	return fmt.Errorf("unsupported automation command %q", sub)
}

func writeAutomationList(c *remote.Client, opts automationFlags, out io.Writer) error {
	response, err := c.Automations()
	if err != nil {
		return err
	}
	if response.Available {
		response.Definitions = cli.FilterAutomations(response.Definitions, cli.AutomationFilter{State: opts.state, Query: opts.query, Project: opts.project, Agent: opts.agent})
	}
	if opts.json {
		if err := json.NewEncoder(out).Encode(response); err != nil {
			return err
		}
	}
	if !response.Available {
		return fmt.Errorf("automation storage unavailable: %s", response.Error)
	}
	if opts.json {
		return nil
	}
	return cli.WriteAutomations(out, response.Definitions)
}

func writeAutomationPreview(c *remote.Client, opts automationFlags, out io.Writer) error {
	response, err := c.PreviewAutomation(opts.spec.Schedule, opts.spec.Timezone)
	if err != nil {
		return err
	}
	if opts.json {
		return json.NewEncoder(out).Encode(response)
	}
	if _, err := fmt.Fprintln(out, response.Human); err != nil {
		return err
	}
	for _, at := range response.Occurrences {
		if _, err := fmt.Fprintln(out, at.Format("2006-01-02T15:04:05Z07:00")); err != nil {
			return err
		}
	}
	return nil
}

func writeAutomationLog(c *remote.Client, id string, opts automationFlags, out io.Writer) error {
	if opts.raw {
		data, err := c.RawAutomationLogs(id, opts.run)
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	}
	response, err := c.AutomationLogs(id, opts.run)
	if err != nil {
		return err
	}
	if opts.json {
		return json.NewEncoder(out).Encode(response)
	}
	return cli.WriteAutomationLogs(out, response.Lines)
}

func writeAutomationTerminal(w io.Writer, target any) error {
	data, err := json.MarshalIndent(target, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
