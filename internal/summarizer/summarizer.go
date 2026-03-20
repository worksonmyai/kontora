package summarizer

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"text/template"
)

// Params holds everything needed to run a summarization.
type Params struct {
	Binary   string
	Args     []string
	Prompt   string
	PaneText string
	TicketID string
	Stage    string
}

// Run executes the summarizer binary with the rendered prompt appended to args,
// piping paneText to stdin, and returns stdout.
func Run(ctx context.Context, p Params) (string, error) {
	prompt, err := renderPrompt(p.Prompt, p.TicketID, p.Stage)
	if err != nil {
		return "", fmt.Errorf("rendering prompt template: %w", err)
	}

	args := make([]string, len(p.Args))
	copy(args, p.Args)
	if prompt != "" {
		args = append(args, prompt)
	}

	cmd := exec.CommandContext(ctx, p.Binary, args...)
	cmd.Stdin = strings.NewReader(p.PaneText)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("summarizer %s: %s: %w", p.Binary, strings.TrimSpace(stderr.String()), err)
	}

	return strings.TrimSpace(stdout.String()), nil
}

type promptData struct {
	TicketID string
	Stage    string
}

func renderPrompt(tmpl, ticketID, stage string) (string, error) {
	if tmpl == "" {
		return "", nil
	}
	t, err := template.New("prompt").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, promptData{TicketID: ticketID, Stage: stage}); err != nil {
		return "", err
	}
	return buf.String(), nil
}
