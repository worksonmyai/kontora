package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

type TicketData struct {
	ID          string
	Title       string
	Description string
	FilePath    string
}

type Data struct {
	Ticket TicketData
}

func Render(tmpl string, data Data, workDir string) (string, error) {
	funcMap := template.FuncMap{
		"file": func(name string) (string, error) {
			b, err := os.ReadFile(filepath.Join(workDir, name))
			if err != nil {
				return "", fmt.Errorf("file %q: %w", name, err)
			}
			return string(b), nil
		},
	}

	t, err := template.New("prompt").Funcs(funcMap).Parse(tmpl)
	if err != nil {
		return "", err
	}

	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
