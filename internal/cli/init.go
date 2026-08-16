package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
)

// pickOneFn is the function used to prompt the user for a single choice.
// Tests can override this to avoid blocking on bubbletea TUI input.
var pickOneFn = pickOne

// pickOneDescsFn is like pickOneFn but supports optional descriptions.
var pickOneDescsFn = pickOneDescs

// EnableOpts carries the fields `kontora init` can supply up front. A set field
// is written to the ticket before the pickers run, so a caller that names every
// one of them never needs a terminal. "none" opts a field out of the project
// default, the same as it does on `kontora new`.
type EnableOpts struct {
	Pipeline string
	Path     string
	Agent    string
	Stage    string
	Status   string
}

// Enable validates a ticket file, fills in required fields from opts and
// interactive pickers, then sets kontora: true to opt it in for daemon
// processing.
func Enable(cfg *config.Config, taskID string, opts EnableOpts, w io.Writer) error {
	tasksDir := config.ExpandTilde(cfg.TicketsDir)
	resolved, err := resolveTaskID(tasksDir, taskID)
	if err != nil {
		return err
	}

	path := filepath.Join(tasksDir, resolved+".md")
	t, err := ticket.ParseFile(path)
	if err != nil {
		return fmt.Errorf("parsing ticket: %w", err)
	}

	if t.Kontora {
		fmt.Fprintf(w, "%s %s\n", styleFaint.Render(resolved), "is already initialized")
		return nil
	}

	if err := applyEnableOpts(cfg, t, opts); err != nil {
		return err
	}

	if t.Path == "" {
		return fmt.Errorf("ticket %s has no path set — pass --path, or add 'path: ~/projects/...' to frontmatter", resolved)
	}

	// A ticket that already says "none" asked for standalone mode; the picker
	// would put a pipeline back.
	standalone := t.Pipeline == config.NoneSentinel
	if err := applyProjectDefaults(cfg, t, w); err != nil {
		return err
	}

	if t.Pipeline == "" && !standalone {
		names := sortedKeys(cfg.Pipelines)
		descs := pipelineDescs(cfg, names)
		val, err := pickOneDescsFn("pipeline", names, descs)
		if err != nil {
			return err
		}
		if err := t.SetField("pipeline", val); err != nil {
			return fmt.Errorf("setting pipeline: %w", err)
		}
	}

	if t.Stage == "" {
		stage, err := startingStage(cfg, t.Pipeline, opts)
		if err != nil {
			return err
		}
		if stage != "" {
			if err := t.SetField("stage", stage); err != nil {
				return fmt.Errorf("setting stage: %w", err)
			}
		}
	}

	val := opts.Status
	if val == "" {
		val, err = pickOneFn("status", []string{string(ticket.StatusOpen), string(ticket.StatusTodo)})
		if err != nil {
			return err
		}
	} else if val != string(ticket.StatusOpen) && val != string(ticket.StatusTodo) {
		return fmt.Errorf("init status must be %q or %q, got %q", ticket.StatusOpen, ticket.StatusTodo, val)
	}
	if val == string(ticket.StatusTodo) {
		// Empty base: init is not a creation surface, so an unresolvable
		// base_branch on the ticket stays uncaught here and pauses at pickup.
		if err := CheckRepo(t.Path, ""); err != nil {
			return err
		}
	}
	if err := t.SetField("status", val); err != nil {
		return fmt.Errorf("setting status: %w", err)
	}

	if err := t.SetField("kontora", true); err != nil {
		return fmt.Errorf("setting kontora: %w", err)
	}

	out, err := t.Marshal()
	if err != nil {
		return fmt.Errorf("marshalling ticket: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("writing ticket file: %w", err)
	}

	fmt.Fprintf(w, "%s %s\n", styleFaint.Render(resolved), styleOK.Render("initialized"))
	return nil
}

// startingStage decides which pipeline stage a freshly initialized ticket
// starts on, or "" when it has no pipeline to run. A caller that pinned the
// pipeline on the command line is scripting init, so it gets the first stage
// rather than a picker it has no terminal to answer.
func startingStage(cfg *config.Config, pipelineName string, opts EnableOpts) (string, error) {
	steps, ok := cfg.Pipelines[pipelineName]
	if !ok || len(steps) == 0 {
		if opts.Stage != "" {
			return "", fmt.Errorf("ticket has no pipeline, so it has no stage to start on")
		}
		return "", nil
	}

	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Stage
	}

	switch {
	case opts.Stage != "":
		if !slices.Contains(names, opts.Stage) {
			return "", fmt.Errorf("stage %q not found in pipeline %q", opts.Stage, pipelineName)
		}
		return opts.Stage, nil
	case opts.Pipeline != "":
		return names[0], nil
	case len(names) > 1:
		return pickOneFn("starting stage", names)
	default:
		return "", nil
	}
}

// applyEnableOpts writes the fields the caller named onto the ticket. The
// sentinel is written through rather than cleared here, because the standalone
// check and applyProjectDefaults both still have to see it. A name the config
// does not know is rejected now instead of sitting in the frontmatter until the
// daemon picks the ticket up and pauses it.
func applyEnableOpts(cfg *config.Config, t *ticket.Ticket, opts EnableOpts) error {
	if opts.Path != "" {
		if err := t.SetField("path", opts.Path); err != nil {
			return fmt.Errorf("setting path: %w", err)
		}
	}
	if opts.Pipeline != "" {
		if name := config.ClearNone(opts.Pipeline); name != "" {
			if _, ok := cfg.Pipelines[name]; !ok {
				return fmt.Errorf("unknown pipeline %q", name)
			}
		}
		if err := t.SetField("pipeline", opts.Pipeline); err != nil {
			return fmt.Errorf("setting pipeline: %w", err)
		}
	}
	if opts.Agent != "" {
		if name := config.ClearNone(opts.Agent); name != "" {
			if _, ok := cfg.Agents[name]; !ok {
				return fmt.Errorf("%w %q", app.ErrUnknownAgent, name)
			}
		}
		if err := t.SetField("agent", opts.Agent); err != nil {
			return fmt.Errorf("setting agent: %w", err)
		}
	}
	return nil
}

// applyProjectDefaults fills the ticket's blank pipeline and agent from the
// project configured for its path and clears the "none" opt-out, so neither the
// picker nor the frontmatter ever sees the sentinel.
func applyProjectDefaults(cfg *config.Config, t *ticket.Ticket, w io.Writer) error {
	pipeline, agent := cfg.ApplyProjectDefaults(t.Path, t.Pipeline, t.Agent)
	if pipeline == t.Pipeline && agent == t.Agent {
		return nil
	}

	var taken []string
	if pipeline != t.Pipeline {
		if err := t.SetField("pipeline", pipeline); err != nil {
			return fmt.Errorf("setting pipeline: %w", err)
		}
		if pipeline != "" {
			taken = append(taken, "pipeline "+pipeline)
		}
	}
	if agent != t.Agent {
		if err := t.SetField("agent", agent); err != nil {
			return fmt.Errorf("setting agent: %w", err)
		}
		if agent != "" {
			taken = append(taken, "agent "+agent)
		}
	}
	if name, _, ok := cfg.ProjectFor(t.Path); ok && len(taken) > 0 {
		fmt.Fprintf(w, "%s %s\n", styleFaint.Render("project "+name), strings.Join(taken, " · "))
	}
	return nil
}

func pickOne(field string, choices []string) (string, error) {
	if len(choices) == 0 {
		return "", fmt.Errorf("no %s configured", field)
	}
	m := pickModel{
		field:   field,
		choices: choices,
	}
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	final := result.(pickModel)
	if final.cancelled {
		return "", ErrCancelled
	}
	return final.choices[final.cursor], nil
}

type pickModel struct {
	field     string
	choices   []string
	descs     []string
	cursor    int
	cancelled bool
}

func (m pickModel) Init() tea.Cmd { return nil }

func (m pickModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyMsg); ok {
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "enter":
			return m, tea.Quit
		case "q", "esc", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m pickModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Select %s:\n\n", styleBold.Render(m.field))
	for i, c := range m.choices {
		desc := ""
		if i < len(m.descs) && m.descs[i] != "" {
			desc = "  " + styleFaint.Render(m.descs[i])
		}
		if i == m.cursor {
			fmt.Fprintf(&b, "  %s %s%s\n", styleCyan.Render("▸"), styleBold.Render(c), desc)
		} else {
			fmt.Fprintf(&b, "    %s%s\n", styleFaint.Render(c), desc)
		}
	}
	b.WriteString("\n")
	b.WriteString(styleFaint.Render("  j/k navigate · enter select · q cancel"))
	b.WriteString("\n")
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pickOneDescs(field string, choices, descs []string) (string, error) {
	if len(choices) == 0 {
		return "", fmt.Errorf("no %s configured", field)
	}
	m := pickModel{
		field:   field,
		choices: choices,
		descs:   descs,
	}
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	final := result.(pickModel)
	if final.cancelled {
		return "", ErrCancelled
	}
	return final.choices[final.cursor], nil
}

func pipelineDescs(cfg *config.Config, names []string) []string {
	descs := make([]string, len(names))
	for i, name := range names {
		stages := cfg.Pipelines[name]
		stageNames := make([]string, len(stages))
		for j, s := range stages {
			stageNames[j] = s.Stage
		}
		descs[i] = strings.Join(stageNames, " → ")
	}
	return descs
}
