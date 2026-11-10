package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

type AutomationContext struct {
	ID        string
	Key       string
	Name      string
	Prompt    string
	Path      string
	Agent     string
	Model     string
	Schedule  string
	Timezone  string
	Workspace string
	Overlap   string
	Timeout   string
	Enabled   bool
}

type AutomationRunContext struct {
	Number       uint64
	N            uint64
	InvocationID string
	Branch       string
	Date         string
	Trigger      string
	ScheduledFor time.Time
	AcceptedAt   time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Outcome      string
	Result       string
	ExitCode     *int
	Cost         *string
}

type AutomationData struct {
	Automation AutomationContext
	Run        AutomationRunContext
	LastRun    AutomationRunContext
}

func fileHelper(workDir string) func(string) (string, error) {
	return func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(workDir, name))
		if err != nil {
			return "", fmt.Errorf("file %q: %w", name, err)
		}
		return string(b), nil
	}
}

// CompileAutomation checks syntax without reading files or executing template actions.
func CompileAutomation(tmpl string) (*template.Template, error) {
	return template.New("automation").Option("missingkey=error").Funcs(template.FuncMap{
		"file": func(string) (string, error) { return "", fmt.Errorf("file helper needs a working directory") },
	}).Parse(tmpl)
}

func RenderAutomation(tmpl string, data AutomationData, workDir string) (string, error) {
	t, err := CompileAutomation(tmpl)
	if err != nil {
		return "", err
	}
	t.Funcs(template.FuncMap{"file": fileHelper(workDir)})
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
