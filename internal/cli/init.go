package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// pickOneFn is the function used to prompt the user for a single choice.
// Tests can override this to avoid blocking on bubbletea TUI input.
var pickOneFn = pickOne

// pickOneDescsFn is like pickOneFn but supports optional descriptions.
var pickOneDescsFn = pickOneDescs

// Enable validates a ticket file and interactively fills in missing required fields,
// then sets kontora: true to opt it in for daemon processing.
func Enable(cfg *config.Config, taskID string, w io.Writer) error {
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

	if t.Path == "" {
		return fmt.Errorf("ticket %s has no path set — add 'path: ~/projects/...' to frontmatter", resolved)
	}

	if t.Pipeline == "" {
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

	if t.Role == "" {
		if pipeline, ok := cfg.Pipelines[t.Pipeline]; ok && len(pipeline) > 1 {
			stages := make([]string, len(pipeline))
			for i, s := range pipeline {
				stages[i] = s.Role
			}
			val, err := pickOneFn("starting stage", stages)
			if err != nil {
				return err
			}
			if err := t.SetField("role", val); err != nil {
				return fmt.Errorf("setting role: %w", err)
			}
		}
	}

	val, err := pickOneFn("status", []string{string(ticket.StatusOpen), string(ticket.StatusTodo)})
	if err != nil {
		return err
	}
	if val == string(ticket.StatusTodo) {
		if err := CheckRepo(t.Path); err != nil {
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
		roles := make([]string, len(stages))
		for j, s := range stages {
			roles[j] = s.Role
		}
		descs[i] = strings.Join(roles, " → ")
	}
	return descs
}
