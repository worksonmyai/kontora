package cli

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/worksonmyai/kontora/internal/config"
)

// agentSetupPrompt is the brief `kontora setup --agent` prints. Embedding it
// ties the instructions to the binary that prints them, so a coding agent never
// follows a schema newer or older than the installed version.
//
//go:embed setup_agent_prompt.md
var agentSetupPrompt string

// Config states reported at the top of the brief.
const (
	configStateMissing = "missing"
	configStateValid   = "valid"
	configStateInvalid = "invalid"
)

// agentSetupContext is the local fact sheet the brief is rendered against. It
// carries no file contents: the brief is printed to a terminal and often piped
// into a second tool, so a token or an environment variable copied in here
// would leak. ValidationError is the one place config values can surface, and
// only the ones the loader quotes back (names, paths, patterns) — never a
// secret's value, because the loader accepts any scalar into a string field
// and so has nothing to complain about.
type agentSetupContext struct {
	ConfigPath      string
	ConfigDir       string
	State           string
	ValidationError string
	SymlinkTarget   string
}

func (c agentSetupContext) Missing() bool { return c.State == configStateMissing }
func (c agentSetupContext) Valid() bool   { return c.State == configStateValid }
func (c agentSetupContext) Invalid() bool { return c.State == configStateInvalid }

// agentSetupTmpl renders the brief. The delimiters are [[ ]] so the stage
// prompt examples the brief contains keep their literal {{ .Ticket.Description }}
// syntax.
var agentSetupTmpl = template.Must(
	template.New("setup-agent").Delims("[[", "]]").Parse(agentSetupPrompt),
)

// WriteAgentSetupPrompt writes plain-text instructions for a coding agent that
// will create or refine the config at configPath. It reads the config to
// classify its state and never writes anything itself.
func WriteAgentSetupPrompt(configPath string, w io.Writer) error {
	if err := agentSetupTmpl.Execute(w, agentSetupContextFor(configPath)); err != nil {
		return fmt.Errorf("rendering setup brief: %w", err)
	}
	return nil
}

func agentSetupContextFor(configPath string) agentSetupContext {
	path := configPath
	if abs, err := filepath.Abs(configPath); err == nil {
		path = abs
	}

	c := agentSetupContext{ConfigPath: path, ConfigDir: filepath.Dir(path)}

	// Readlink errors on anything that is not a symlink, so it doubles as the
	// test. A config reached through a dotfiles symlink has to be edited at its
	// target, or the write replaces the link.
	if target, err := os.Readlink(path); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		c.SymlinkTarget = filepath.Clean(target)
	}

	switch _, err := config.Load(path); {
	case err == nil:
		c.State = configStateValid
	case errors.Is(err, config.ErrNotFound):
		c.State = configStateMissing
	default:
		c.State = configStateInvalid
		// A YAML error spans several lines. The brief prints it in an indented
		// block, so the continuation lines have to carry the same indent or the
		// block ends after the first one.
		c.ValidationError = strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", "\n    ")
	}
	return c
}
