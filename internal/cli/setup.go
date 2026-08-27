package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/worksonmyai/kontora/internal/config"
)

var knownAgents = map[string]agentInfo{
	"claude": {
		binary:      "claude",
		defaultArgs: "--dangerously-skip-permissions",
	},
	"pi": {
		binary:      "pi",
		defaultArgs: "-p --no-session",
	},
	"opencode": {
		binary:      "opencode",
		defaultArgs: "",
	},
}

// knownAgentOrder defines the display order for agents in the wizard.
var knownAgentOrder = []string{"claude", "pi", "opencode"}

type agentInfo struct {
	binary      string
	defaultArgs string
}

// SetupAnswers holds all user choices from the setup wizard.
type SetupAnswers struct {
	Agents              map[string]agentArgs // name -> args
	TicketsDir          string
	LogsDir             string
	WorktreesDir        string
	MaxConcurrentAgents int
	WebEnabled          bool
	WebPort             int
	DefaultPipeline     string // "" runs a new ticket as one agent pass
}

// setupPipelines are the pipelines the wizard writes, in the order the
// pipelines step lists them, each with the lines shown under it. The empty
// name is the "neither" choice.
var setupPipelines = []struct {
	name string
	desc []string
}{
	{
		name: "default",
		desc: []string{
			"Single-stage: sends the ticket description to the agent and runs",
			"it to completion. Good for simple, self-contained tickets.",
		},
	},
	{
		name: "implement-review-commit",
		desc: []string{
			"Multi-stage pipeline: implement → review → fix-review → commit.",
			"The agent implements the ticket, a second pass reviews the code,",
			"a third pass fixes any issues found, then commits the result.",
		},
	},
	{
		desc: []string{
			"New tickets run one agent on their description. Both pipelines",
			"stay in the config, for --pipeline on a single ticket.",
		},
	},
}

type agentArgs struct {
	Binary string
	Args   string
}

const (
	stepAgents      = 0
	stepArgs        = 1
	stepDirs        = 2
	stepSettings    = 3
	stepPipelines   = 4
	stepConfirm     = 5
	totalSetupSteps = 6
)

type setupModel struct {
	step      int
	cancelled bool
	done      bool

	// Step 1: agent selection
	agentNames   []string // ordered list of all known agents
	agentOnPath  map[string]bool
	agentChecked map[string]bool
	agentCursor  int

	// Step 2: agent args
	selectedAgents []string // ordered list of selected agents
	argsIndex      int      // which agent we're editing
	argsInputs     map[string]textinput.Model
	argsEditing    bool

	// Step 3: directories
	dirFields   [3]textinput.Model // tickets, logs, worktrees
	dirCursor   int
	dirEditing  bool
	dirLabels   [3]string
	dirDescs    [3]string
	dirDefaults [3]string

	// Step 4: settings
	maxConcurrent  textinput.Model
	webEnabled     bool
	webPort        textinput.Model
	settingCursor  int
	settingEditing bool

	// Step 5: which pipeline new tickets run
	pipelineCursor int

	// Error message
	err string
}

func initialSetupModel() setupModel {
	agentOnPath := make(map[string]bool)
	agentChecked := make(map[string]bool)
	for _, name := range knownAgentOrder {
		info := knownAgents[name]
		if _, err := exec.LookPath(info.binary); err == nil {
			agentOnPath[name] = true
			agentChecked[name] = true
		}
	}

	return setupModel{
		step:         stepAgents,
		agentNames:   knownAgentOrder,
		agentOnPath:  agentOnPath,
		agentChecked: agentChecked,
		argsInputs:   make(map[string]textinput.Model),
		dirLabels:    [3]string{"tickets_dir", "logs_dir", "worktrees_dir"},
		dirDescs:     [3]string{"where ticket markdown files are stored", "where agent stdout/stderr logs go", "base directory for git worktrees created per ticket"},
		dirDefaults:  [3]string{"~/.kontora/tickets", "~/.kontora/logs", "~/.kontora/worktrees"},
		dirFields: [3]textinput.Model{
			newSetupInput("~/.kontora/tickets"),
			newSetupInput("~/.kontora/logs"),
			newSetupInput("~/.kontora/worktrees"),
		},
		maxConcurrent: newSetupInput("3"),
		webEnabled:    true,
		webPort:       newSetupInput("8080"),
	}
}

// newSetupInput builds one wizard text field. The wizard draws its own labels,
// so the input contributes only the value and the cursor.
func newSetupInput(value string) textinput.Model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.SetValue(value)
	return ti
}

func (m setupModel) Init() tea.Cmd { return nil }

func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		// The cursor blink arrives this way, so the focused field still needs it.
		return m.forwardToInput(msg)
	}

	if key.String() == "ctrl+c" {
		m.cancelled = true
		return m, tea.Quit
	}

	var next setupModel
	var cmd tea.Cmd
	switch m.step {
	case stepAgents:
		next, cmd = m.updateAgents(key)
	case stepArgs:
		next, cmd = m.updateArgs(key)
	case stepDirs:
		next, cmd = m.updateDirs(key)
	case stepSettings:
		next, cmd = m.updateSettings(key)
	case stepPipelines:
		next, cmd = m.updatePipelines(key)
	case stepConfirm:
		next, cmd = m.updateConfirm(key)
	default:
		return m, nil
	}
	// syncFocus mutates next, so it has to run before next is returned rather
	// than as an argument alongside it.
	focus := next.syncFocus()
	return next, tea.Batch(cmd, focus)
}

// editing returns the text field the wizard's current position edits, and
// whether it has one.
func (m *setupModel) editing() (*textinput.Model, bool) {
	switch {
	case m.step == stepArgs && m.argsEditing && m.argsIndex < len(m.selectedAgents):
		// The map value is not addressable, so the caller writes it back.
		ti := m.argsInputs[m.selectedAgents[m.argsIndex]]
		return &ti, true
	case m.step == stepDirs && m.dirEditing:
		return &m.dirFields[m.dirCursor], true
	case m.step == stepSettings && m.settingEditing && m.settingCursor == 0:
		return &m.maxConcurrent, true
	case m.step == stepSettings && m.settingEditing && m.settingCursor == 2:
		return &m.webPort, true
	}
	return nil, false
}

// storeEditing completes an editing call that had to return a copy.
func (m *setupModel) storeEditing(ti *textinput.Model) {
	if m.step == stepArgs && m.argsEditing && m.argsIndex < len(m.selectedAgents) {
		m.argsInputs[m.selectedAgents[m.argsIndex]] = *ti
	}
}

// forwardToInput hands msg to the field being edited, and drops it when no
// field is.
func (m setupModel) forwardToInput(msg tea.Msg) (setupModel, tea.Cmd) {
	ti, ok := m.editing()
	if !ok {
		return m, nil
	}
	before := ti.Value()
	var cmd tea.Cmd
	*ti, cmd = ti.Update(msg)
	// Checking the resulting value rather than the keystroke: text reaches a
	// field several characters at a time (a fast burst coalesced into one
	// event, a bracketed paste, ctrl+v, a multi-byte rune), and answers()
	// silently falls back to the default on a value it cannot parse, so a
	// rejected keystroke is the only way the confirm screen and the written
	// config stay the same number.
	if m.numericEditing() && !isDigits(ti.Value()) {
		ti.SetValue(before)
	}
	m.storeEditing(ti)
	return m, cmd
}

// numericEditing reports whether the field being edited holds a number.
func (m *setupModel) numericEditing() bool {
	return m.step == stepSettings && m.settingEditing && (m.settingCursor == 0 || m.settingCursor == 2)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// syncFocus gives the keyboard cursor to the field the wizard is editing and
// takes it from every other one, so a step handler only has to move the cursor
// and set its editing flag.
func (m *setupModel) syncFocus() tea.Cmd {
	for name, ti := range m.argsInputs {
		ti.Blur()
		m.argsInputs[name] = ti
	}
	for i := range m.dirFields {
		m.dirFields[i].Blur()
	}
	m.maxConcurrent.Blur()
	m.webPort.Blur()

	ti, ok := m.editing()
	if !ok {
		return nil
	}
	cmd := ti.Focus()
	m.storeEditing(ti)
	return cmd
}

func (m setupModel) updateAgents(key tea.KeyMsg) (setupModel, tea.Cmd) {
	switch key.String() {
	case "j", "down":
		if m.agentCursor < len(m.agentNames)-1 {
			m.agentCursor++
		}
	case "k", "up":
		if m.agentCursor > 0 {
			m.agentCursor--
		}
	case " ":
		name := m.agentNames[m.agentCursor]
		m.agentChecked[name] = !m.agentChecked[name]
	case "enter":
		var selected []string
		for _, name := range m.agentNames {
			if m.agentChecked[name] {
				selected = append(selected, name)
			}
		}
		if len(selected) == 0 {
			m.err = "select at least one agent"
			return m, nil
		}
		m.err = ""
		m.selectedAgents = selected
		for _, name := range selected {
			if _, ok := m.argsInputs[name]; !ok {
				m.argsInputs[name] = newSetupInput(knownAgents[name].defaultArgs)
			}
		}
		m.argsIndex = 0
		m.argsEditing = true
		m.step = stepArgs
	case "q", "esc":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m setupModel) updateArgs(key tea.KeyMsg) (setupModel, tea.Cmd) {
	if !m.argsEditing {
		switch key.String() {
		case "j", "down":
			if m.argsIndex < len(m.selectedAgents)-1 {
				m.argsIndex++
			}
		case "k", "up":
			if m.argsIndex > 0 {
				m.argsIndex--
			}
		case "shift+tab":
			if m.argsIndex > 0 {
				m.argsIndex--
			} else {
				m.step = stepAgents
			}
		case "enter":
			m.argsEditing = true
		}
		return m, nil
	}

	switch key.String() {
	case "enter":
		m.argsEditing = false
		m.argsIndex++
		if m.argsIndex >= len(m.selectedAgents) {
			m.step = stepDirs
			m.dirCursor = 0
			m.dirEditing = true
			return m, nil
		}
		m.argsEditing = true
	case "shift+tab":
		if m.argsIndex > 0 {
			m.argsIndex--
		} else {
			m.step = stepAgents
			m.argsEditing = false
		}
	case "esc":
		// Leave the field, not the wizard: cancelling here would throw away
		// every answer given so far. ctrl+c is still the way out.
		m.argsEditing = false
	default:
		return m.forwardToInput(key)
	}
	return m, nil
}

func (m setupModel) updateDirs(key tea.KeyMsg) (setupModel, tea.Cmd) {
	if m.dirEditing {
		switch key.String() {
		case "enter", "tab":
			m.dirEditing = false
			m.dirCursor++
			if m.dirCursor >= 3 {
				m.step = stepSettings
				m.settingCursor = 0
				m.settingEditing = true
				return m, nil
			}
			m.dirEditing = true
		case "shift+tab":
			if m.dirCursor > 0 {
				m.dirCursor--
			} else {
				m.step = stepArgs
				m.argsIndex = len(m.selectedAgents) - 1
				m.argsEditing = true
			}
		case "esc":
			m.dirEditing = false
		default:
			return m.forwardToInput(key)
		}
		return m, nil
	}

	switch key.String() {
	case "j", "down":
		if m.dirCursor < 2 {
			m.dirCursor++
		}
	case "k", "up":
		if m.dirCursor > 0 {
			m.dirCursor--
		}
	case "shift+tab":
		if m.dirCursor > 0 {
			m.dirCursor--
		} else {
			m.step = stepArgs
			m.argsIndex = len(m.selectedAgents) - 1
		}
	case "enter":
		m.dirEditing = true
	}
	return m, nil
}

func (m setupModel) updateSettings(key tea.KeyMsg) (setupModel, tea.Cmd) {
	// Settings: 0=max_concurrent, 1=web_enabled, 2=web_port
	if m.settingEditing && m.settingCursor != 1 {
		switch key.String() {
		case "enter":
			m.settingEditing = false
			m.settingCursor++
			if m.settingCursor == 1 {
				// web_enabled is a toggle, not text
				m.settingEditing = false
			} else if m.settingCursor >= 3 {
				m.step = stepPipelines
				return m, nil
			} else {
				m.settingEditing = true
			}
		case "shift+tab":
			if m.settingCursor > 0 {
				m.settingCursor--
				m.settingEditing = m.settingCursor != 1
			} else {
				m.step = stepDirs
				m.dirCursor = 2
				m.dirEditing = true
				m.settingEditing = false
			}
		case "esc":
			m.settingEditing = false
		default:
			// Drop a plain non-digit here rather than in forwardToInput, which
			// would take the cursor to the end of the field to undo it.
			// Editing keys are longer than one character, so they still reach
			// the field.
			if k := key.String(); len(k) == 1 && (k < "0" || k > "9") {
				return m, nil
			}
			return m.forwardToInput(key)
		}
		return m, nil
	}

	switch key.String() {
	case "j", "down":
		if m.settingCursor < 2 {
			m.settingCursor++
		}
	case "k", "up":
		if m.settingCursor > 0 {
			m.settingCursor--
		}
	case "shift+tab":
		if m.settingCursor > 0 {
			m.settingCursor--
		} else {
			m.step = stepDirs
			m.dirCursor = 2
			m.dirEditing = true
		}
	case "enter":
		if m.settingCursor == 1 {
			// Toggle web enabled
			m.settingEditing = false
			m.settingCursor++
			if m.settingCursor >= 3 {
				m.step = stepConfirm
				return m, nil
			}
			m.settingEditing = true
		} else {
			m.settingEditing = true
		}
	case " ":
		if m.settingCursor == 1 {
			m.webEnabled = !m.webEnabled
		}
	}
	return m, nil
}

func (m setupModel) updatePipelines(key tea.KeyMsg) (setupModel, tea.Cmd) {
	switch key.String() {
	case "j", "down":
		if m.pipelineCursor < len(setupPipelines)-1 {
			m.pipelineCursor++
		}
	case "k", "up":
		if m.pipelineCursor > 0 {
			m.pipelineCursor--
		}
	case "enter":
		m.step = stepConfirm
	case "shift+tab":
		m.step = stepSettings
		m.settingCursor = 2
		m.settingEditing = true
	case "q", "esc":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m setupModel) updateConfirm(key tea.KeyMsg) (setupModel, tea.Cmd) {
	switch key.String() {
	case "enter", "y":
		m.done = true
		return m, tea.Quit
	case "shift+tab":
		m.step = stepPipelines
	case "q", "esc", "n":
		m.cancelled = true
		return m, tea.Quit
	}
	return m, nil
}

func (m setupModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d/%d)\n\n", styleBold.Render("kontora setup"), m.step+1, totalSetupSteps)

	switch m.step {
	case stepAgents:
		m.viewAgents(&b)
	case stepArgs:
		m.viewArgs(&b)
	case stepDirs:
		m.viewDirs(&b)
	case stepSettings:
		m.viewSettings(&b)
	case stepPipelines:
		m.viewPipelines(&b)
	case stepConfirm:
		m.viewConfirm(&b)
	}
	return b.String()
}

func (m setupModel) viewAgents(b *strings.Builder) {
	b.WriteString(styleBold.Render("Select agents to configure:") + "\n")
	b.WriteString(styleFaint.Render("  Agents are the AI coding tools kontora will orchestrate.") + "\n\n")
	for i, name := range m.agentNames {
		cursor := "  "
		if i == m.agentCursor {
			cursor = styleCyan.Render("▸ ")
		}
		check := "[ ]"
		if m.agentChecked[name] {
			check = styleOK.Render("[✓]")
		}
		var status string
		if m.agentOnPath[name] {
			status = styleOK.Render(" (found on PATH)")
		} else {
			status = styleFaint.Render(" (not found)")
		}
		fmt.Fprintf(b, "%s%s %s%s\n", cursor, check, name, status)
	}
	b.WriteString("\n")
	if m.err != "" {
		fmt.Fprintf(b, "  %s\n", styleFail.Render(m.err))
	}
	b.WriteString(styleFaint.Render("  space toggle · j/k navigate · enter confirm · q cancel") + "\n")
}

func (m setupModel) viewArgs(b *strings.Builder) {
	b.WriteString(styleBold.Render("Agent arguments:") + "\n\n")
	for i, name := range m.selectedAgents {
		prefix := "  "
		if i == m.argsIndex {
			prefix = styleCyan.Render("▸ ")
		}
		val := m.argsInputs[name]
		if i == m.argsIndex && m.argsEditing {
			fmt.Fprintf(b, "%s%s args: %s\n", prefix, styleBold.Render(name), val.View())
		} else if i < m.argsIndex {
			fmt.Fprintf(b, "%s%s args: %s\n", prefix, styleFaint.Render(name), styleFaint.Render(val.Value()))
		} else {
			fmt.Fprintf(b, "%s%s args: %s\n", prefix, name, styleFaint.Render(val.Value()))
		}
	}
	b.WriteString("\n")
	b.WriteString(styleFaint.Render("  type to edit · enter next · shift+tab back · esc leave the field") + "\n")
}

func (m setupModel) viewDirs(b *strings.Builder) {
	b.WriteString(styleBold.Render("Directories:") + "\n")
	b.WriteString(styleFaint.Render("  kontora stores all state as files — no database.") + "\n\n")
	for i := range 3 {
		prefix := "  "
		if i == m.dirCursor {
			prefix = styleCyan.Render("▸ ")
		}
		if i == m.dirCursor && m.dirEditing {
			fmt.Fprintf(b, "%s%s: %s\n", prefix, styleBold.Render(m.dirLabels[i]), m.dirFields[i].View())
			fmt.Fprintf(b, "    %s\n", styleFaint.Render(m.dirDescs[i]))
		} else {
			fmt.Fprintf(b, "%s%s: %s\n", prefix, m.dirLabels[i], styleFaint.Render(m.dirFields[i].Value()))
		}
	}
	b.WriteString("\n")
	b.WriteString(styleFaint.Render("  type to edit · enter/tab next · shift+tab back · esc leave the field") + "\n")
}

func (m setupModel) viewSettings(b *strings.Builder) {
	b.WriteString(styleBold.Render("Settings:") + "\n\n")
	labels := [3]string{"max_concurrent_agents", "web enabled", "web port"}
	descs := [3]string{"how many agents can run in parallel", "start the web UI for monitoring tickets", "HTTP port for the web UI"}
	for i := range 3 {
		prefix := "  "
		if i == m.settingCursor {
			prefix = styleCyan.Render("▸ ")
		}
		switch i {
		case 0:
			if i == m.settingCursor && m.settingEditing {
				fmt.Fprintf(b, "%s%s: %s\n", prefix, styleBold.Render(labels[i]), m.maxConcurrent.View())
				fmt.Fprintf(b, "    %s\n", styleFaint.Render(descs[i]))
			} else {
				fmt.Fprintf(b, "%s%s: %s\n", prefix, labels[i], styleFaint.Render(m.maxConcurrent.Value()))
			}
		case 1:
			val := "yes"
			if !m.webEnabled {
				val = "no"
			}
			if i == m.settingCursor {
				fmt.Fprintf(b, "%s%s: %s\n", prefix, styleBold.Render(labels[i]), val)
				fmt.Fprintf(b, "    %s\n", styleFaint.Render(descs[i]))
			} else {
				fmt.Fprintf(b, "%s%s: %s\n", prefix, labels[i], styleFaint.Render(val))
			}
		case 2:
			if i == m.settingCursor && m.settingEditing {
				fmt.Fprintf(b, "%s%s: %s\n", prefix, styleBold.Render(labels[i]), m.webPort.View())
				fmt.Fprintf(b, "    %s\n", styleFaint.Render(descs[i]))
			} else {
				fmt.Fprintf(b, "%s%s: %s\n", prefix, labels[i], styleFaint.Render(m.webPort.Value()))
			}
		}
	}
	b.WriteString("\n")
	b.WriteString(styleFaint.Render("  space toggle · enter next · shift+tab back · esc leave the field") + "\n")
}

func (m setupModel) viewPipelines(b *strings.Builder) {
	b.WriteString(styleBold.Render("Pipelines:") + "\n")
	b.WriteString(styleFaint.Render("  Pipelines define multi-stage workflows for tickets. Both starter") + "\n")
	b.WriteString(styleFaint.Render("  pipelines go into config.yaml. Which one do new tickets run?") + "\n\n")

	for i, p := range setupPipelines {
		cursor := "  "
		if i == m.pipelineCursor {
			cursor = styleCyan.Render("▸ ")
		}
		mark := "( )"
		if i == m.pipelineCursor {
			mark = styleOK.Render("(•)")
		}
		label := p.name
		if label == "" {
			label = "neither"
		}
		fmt.Fprintf(b, "%s%s %s\n", cursor, mark, styleBold.Render(label))
		for _, line := range p.desc {
			b.WriteString("      " + styleFaint.Render(line) + "\n")
		}
		b.WriteString("\n")
	}

	b.WriteString(styleFaint.Render("  j/k navigate · enter confirm · shift+tab back · esc cancel") + "\n")
}

func (m setupModel) viewConfirm(b *strings.Builder) {
	b.WriteString(styleBold.Render("Summary:") + "\n\n")
	b.WriteString("  Agents:\n")
	for _, name := range m.selectedAgents {
		args := m.argsInputs[name].Value()
		if args != "" {
			fmt.Fprintf(b, "    %s (%s)\n", styleBold.Render(name), args)
		} else {
			fmt.Fprintf(b, "    %s\n", styleBold.Render(name))
		}
	}
	b.WriteString("\n  Directories:\n")
	for i := range 3 {
		fmt.Fprintf(b, "    %s: %s\n", m.dirLabels[i], m.dirFields[i].Value())
	}
	b.WriteString("\n  Settings:\n")
	fmt.Fprintf(b, "    max_concurrent_agents: %s\n", m.maxConcurrent.Value())
	webStr := "yes"
	if !m.webEnabled {
		webStr = "no"
	}
	fmt.Fprintf(b, "    web: %s (port %s)\n", webStr, m.webPort.Value())
	b.WriteString("\n  Pipelines:\n")
	fmt.Fprintf(b, "    %s, %s\n", "default", "implement-review-commit")
	if p := setupPipelines[m.pipelineCursor].name; p != "" {
		fmt.Fprintf(b, "    new tickets run: %s\n", p)
	} else {
		fmt.Fprintf(b, "    new tickets run: %s\n", "one agent pass, no pipeline")
	}
	b.WriteString("\n")
	b.WriteString(styleFaint.Render("  enter/y write config · shift+tab back · q/n cancel") + "\n")
}

func (m setupModel) answers() *SetupAnswers {
	agents := make(map[string]agentArgs, len(m.selectedAgents))
	for _, name := range m.selectedAgents {
		agents[name] = agentArgs{
			Binary: knownAgents[name].binary,
			Args:   m.argsInputs[name].Value(),
		}
	}

	maxC, _ := strconv.Atoi(m.maxConcurrent.Value())
	if maxC <= 0 {
		maxC = 3
	}
	port, _ := strconv.Atoi(m.webPort.Value())
	if port <= 0 || port > 65535 {
		port = 8080
	}

	return &SetupAnswers{
		Agents:              agents,
		TicketsDir:          m.dirFields[0].Value(),
		LogsDir:             m.dirFields[1].Value(),
		WorktreesDir:        m.dirFields[2].Value(),
		MaxConcurrentAgents: maxC,
		WebEnabled:          m.webEnabled,
		WebPort:             port,
		DefaultPipeline:     setupPipelines[m.pipelineCursor].name,
	}
}

// buildConfigYAML generates YAML config from wizard answers.
func buildConfigYAML(ans *SetupAnswers) string {
	var b strings.Builder

	// Determine default agent (first selected, prefer "claude")
	var defaultAgent string
	agentNames := make([]string, 0, len(ans.Agents))
	for name := range ans.Agents {
		agentNames = append(agentNames, name)
		if name == "claude" {
			defaultAgent = name
		}
	}
	sort.Strings(agentNames)
	if defaultAgent == "" && len(agentNames) > 0 {
		defaultAgent = agentNames[0]
	}

	// Write YAML manually to keep it readable
	if defaultAgent != "" {
		// Config.applyDefaults only infers this for a lone agent or one named
		// "claude", so a setup with pi and opencode has to state it.
		fmt.Fprintf(&b, "default_agent: %s\n", defaultAgent)
	}
	if ans.DefaultPipeline != "" {
		fmt.Fprintf(&b, "default_pipeline: %s\n", ans.DefaultPipeline)
	}
	fmt.Fprintf(&b, "tickets_dir: %s\n", yamlQuote(ans.TicketsDir))
	fmt.Fprintf(&b, "logs_dir: %s\n", yamlQuote(ans.LogsDir))
	fmt.Fprintf(&b, "worktrees_dir: %s\n", yamlQuote(ans.WorktreesDir))
	fmt.Fprintf(&b, "max_concurrent_agents: %d\n", ans.MaxConcurrentAgents)
	b.WriteString("\n")

	fmt.Fprintf(&b, "web:\n")
	fmt.Fprintf(&b, "  enabled: %t\n", ans.WebEnabled)
	fmt.Fprintf(&b, "  port: %d\n", ans.WebPort)
	b.WriteString("\n")

	b.WriteString("agents:\n")
	for _, name := range agentNames {
		a := ans.Agents[name]
		fmt.Fprintf(&b, "  %s:\n", name)
		fmt.Fprintf(&b, "    binary: %s\n", a.Binary)
		if a.Args != "" {
			args := splitArgs(a.Args)
			if len(args) > 0 {
				b.WriteString("    args:\n")
				for _, arg := range args {
					fmt.Fprintf(&b, "      - %s\n", yamlQuote(arg))
				}
			}
		}
	}
	b.WriteString("\n")

	b.WriteString("stages:\n")
	b.WriteString("  code:\n")
	b.WriteString("    prompt: |\n")
	b.WriteString("      {{ .Ticket.Description }}\n")
	b.WriteString("    timeout: 30m\n")
	b.WriteString("\n")
	b.WriteString("  implement:\n")
	b.WriteString("    prompt: |\n")
	b.WriteString("      {{ .Ticket.Description }}\n")
	b.WriteString("\n")
	b.WriteString("      Do NOT commit or push. Only implement the code and run tests.\n")
	b.WriteString("    timeout: 60m\n")
	b.WriteString("\n")
	b.WriteString("  review:\n")
	b.WriteString("    prompt: |\n")
	b.WriteString("      Review the code changes for this ticket. Check for:\n")
	b.WriteString("      - Correctness and edge cases\n")
	b.WriteString("      - Code quality and maintainability\n")
	b.WriteString("      - Test coverage\n")
	b.WriteString("\n")
	b.WriteString("      Write all review results to the ticket {{ .Ticket.ID }}\n")
	b.WriteString("    timeout: 30m\n")
	b.WriteString("\n")
	b.WriteString("  fix-review:\n")
	b.WriteString("    prompt: |\n")
	b.WriteString("      Read the code review from the notes of {{ .Ticket.ID }}\n")
	b.WriteString("\n")
	b.WriteString("      If there are issues listed, fix all of them and run tests.\n")
	b.WriteString("    timeout: 30m\n")
	b.WriteString("\n")
	b.WriteString("  commit:\n")
	b.WriteString("    prompt: |\n")
	b.WriteString("      Commit uncommitted files. This is the ready and reviewed\n")
	b.WriteString("      implementation of the ticket {{ .Ticket.ID }}.\n")
	b.WriteString("\n")
	b.WriteString("      You have full authorization to commit.\n")
	b.WriteString("      Do not ask for confirmation — just do it.\n")
	b.WriteString("    timeout: 5m\n")
	b.WriteString("\n")

	b.WriteString("pipelines:\n")
	b.WriteString("  default:\n")
	fmt.Fprintf(&b, "    - stage: code\n")
	fmt.Fprintf(&b, "      agent: %s\n", defaultAgent)
	fmt.Fprintf(&b, "      on_success: human_review\n")
	fmt.Fprintf(&b, "      on_failure: pause\n")
	b.WriteString("\n")
	b.WriteString("  implement-review-commit:\n")
	fmt.Fprintf(&b, "    - stage: implement\n")
	fmt.Fprintf(&b, "      agent: %s\n", defaultAgent)
	fmt.Fprintf(&b, "      on_success: next\n")
	fmt.Fprintf(&b, "      on_failure: pause\n")
	fmt.Fprintf(&b, "    - stage: review\n")
	fmt.Fprintf(&b, "      agent: %s\n", defaultAgent)
	fmt.Fprintf(&b, "      on_success: next\n")
	fmt.Fprintf(&b, "      on_failure: retry\n")
	fmt.Fprintf(&b, "      max_retries: 1\n")
	fmt.Fprintf(&b, "    - stage: fix-review\n")
	fmt.Fprintf(&b, "      agent: %s\n", defaultAgent)
	fmt.Fprintf(&b, "      on_success: next\n")
	fmt.Fprintf(&b, "      on_failure: retry\n")
	fmt.Fprintf(&b, "      max_retries: 1\n")
	fmt.Fprintf(&b, "    - stage: commit\n")
	fmt.Fprintf(&b, "      agent: %s\n", defaultAgent)
	fmt.Fprintf(&b, "      on_success: human_review\n")
	fmt.Fprintf(&b, "      on_failure: retry\n")
	fmt.Fprintf(&b, "      max_retries: 1\n")

	return b.String()
}

func splitArgs(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// writeSetupConfig writes the config file from wizard answers.
// Creates parent directories as needed.
func writeSetupConfig(configPath string, ans *SetupAnswers, w io.Writer) error {
	if ans.MaxConcurrentAgents <= 0 {
		return fmt.Errorf("max_concurrent_agents must be positive")
	}
	if len(ans.Agents) == 0 {
		return fmt.Errorf("at least one agent is required")
	}

	// Validate before anything touches the filesystem, so a generated config
	// that cannot load leaves no directories behind.
	content := buildConfigYAML(ans)
	if _, err := config.LoadReader(strings.NewReader(content)); err != nil {
		return fmt.Errorf("generated config is invalid (bug): %w", err)
	}

	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	// Create runtime directories
	for _, dir := range []string{ans.TicketsDir, ans.LogsDir, ans.WorktreesDir} {
		expanded := config.ExpandTilde(dir)
		if err := os.MkdirAll(expanded, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	fmt.Fprintf(w, "  %s Config written to %s\n", styleOK.Render("✓"), styleFaint.Render(configPath))
	return nil
}

// RunSetup runs the interactive setup wizard.
func RunSetup(configPath string, w io.Writer) error {
	// Never overwrite: the file on disk is the user's, and setup has no way to
	// know which of its settings were deliberate.
	if _, err := os.Stat(configPath); err == nil {
		fmt.Fprintf(w, "Config already exists: %s\n\n", styleFaint.Render(configPath))
		fmt.Fprintln(w, "To change it:")
		// Pad before styling: the ANSI codes make the rendered string longer
		// than it looks, so a %-Ns on the styled value aligns nothing.
		fmt.Fprintf(w, "  %s  %s\n", styleCyan.Render(fmt.Sprintf("%-21s", "kontora setup --agent")), styleFaint.Render("Print instructions for a coding agent"))
		fmt.Fprintf(w, "  %s  %s\n", styleCyan.Render(fmt.Sprintf("%-21s", "kontora config edit")), styleFaint.Render("Edit it yourself"))
		return nil
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Fprintf(w, "  %s %s\n", styleWarn.Render("!"), "tmux not found on PATH — kontora requires tmux to run agents")
	}

	m := initialSetupModel()
	p := tea.NewProgram(m)
	result, err := p.Run()
	if err != nil {
		return fmt.Errorf("setup wizard: %w", err)
	}

	final := result.(setupModel)
	if final.cancelled {
		fmt.Fprintln(w, "Setup cancelled.")
		return ErrCancelled
	}

	ans := final.answers()
	if err := writeSetupConfig(configPath, ans, w); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, styleBold.Render("Next steps:"))
	fmt.Fprintf(w, "  %s  %s\n", styleCyan.Render("kontora doctor"), styleFaint.Render("Verify setup"))
	fmt.Fprintf(w, "  %s  %s\n", styleCyan.Render("kontora start"), styleFaint.Render("Start the daemon"))
	fmt.Fprintf(w, "  %s  %s\n", styleCyan.Render("kontora new --path ~/projects/myrepo \"My first ticket\""), styleFaint.Render("Create a ticket"))
	fmt.Fprintf(w, "  %s  %s\n", styleCyan.Render("kontora setup --agent"), styleFaint.Render("Refine the config with a coding agent"))

	return nil
}

// runSetupFn is the function used to run the setup wizard.
// Tests can override this to inject canned answers.
var runSetupFn = RunSetup
