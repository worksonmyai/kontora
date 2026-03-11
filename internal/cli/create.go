package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
)

// yamlQuote wraps a string in double quotes if it contains characters
// that are special in YAML (colons, hashes, brackets, etc.).
// Simple alphanumeric values are left unquoted.
func yamlQuote(s string) string {
	if s == "" || strings.ContainsAny(s, ":#{}&*!|>'\"%@`,[]\\") {
		return fmt.Sprintf("%q", s)
	}
	switch strings.ToLower(s) {
	case "null", "~":
		return fmt.Sprintf("%q", s)
	}
	return s
}

type NewOpts struct {
	ID       string // Pre-generated ID; if empty, GenerateID is called.
	Path     string
	Pipeline string
	Agent    string
	Status   string
	Title    string
	Body     string
	NoEdit   bool
}

// New creates a ticket file and optionally opens it in $EDITOR.
func New(cfg *config.Config, opts NewOpts) (string, error) {
	if opts.Path == "" {
		return "", fmt.Errorf("-path is required")
	}
	if opts.Title == "" {
		return "", fmt.Errorf("title is required")
	}
	if opts.Status == "" {
		opts.Status = "todo"
	}

	if opts.Status != "open" {
		if err := CheckRepo(opts.Path); err != nil {
			return "", err
		}
	}

	id := opts.ID
	if id == "" {
		var err error
		id, err = GenerateID(cfg.TicketsDir, opts.Path)
		if err != nil {
			return "", fmt.Errorf("generating id: %w", err)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	pipelineLine := ""
	if opts.Pipeline != "" {
		pipelineLine = fmt.Sprintf("pipeline: %s\n", yamlQuote(opts.Pipeline))
	}
	agentLine := ""
	if opts.Agent != "" {
		agentLine = fmt.Sprintf("agent: %s\n", yamlQuote(opts.Agent))
	}
	body := "\n"
	if opts.Body != "" {
		body = "\n" + opts.Body + "\n"
	}
	content := fmt.Sprintf("---\nid: %s\nkontora: true\nstatus: %s\n%s%spath: %s\ncreated: %s\n---\n# %s\n%s",
		id, yamlQuote(opts.Status), pipelineLine, agentLine, yamlQuote(opts.Path), now, opts.Title, body)

	dir := config.ExpandTilde(cfg.TicketsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating tickets dir: %w", err)
	}
	path := filepath.Join(dir, id+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("writing ticket file: %w", err)
	}

	if !opts.NoEdit {
		if err := openEditor(cfg.Editor, path); err != nil {
			return id, err
		}
	}

	return id, nil
}

type QuickOpts struct {
	Path     string
	Pipeline string
	Title    string
}

// Quick creates a ticket file without opening an editor. Status defaults to todo.
func Quick(cfg *config.Config, opts QuickOpts) (string, error) {
	return New(cfg, NewOpts{
		Path:     opts.Path,
		Pipeline: opts.Pipeline,
		Status:   "todo",
		Title:    opts.Title,
		NoEdit:   true,
	})
}
