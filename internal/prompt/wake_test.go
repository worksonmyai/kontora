package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestRenderWake(t *testing.T) {
	wake := &ticket.WakeContext{Reason: "ready", Output: "Collected 雪\nsecond line", Count: 2, Next: "Read data.\nReport result.", WaitID: "wait-1"}
	data := Data{Ticket: TicketData{ID: "kon-test"}, Wake: wake}
	for _, render := range []func(string, Data, string) (string, error){
		Render,
		func(tmpl string, data Data, dir string) (string, error) {
			return RenderWithOptions(tmpl, data, dir, Options{})
		},
	} {
		out, err := render("Existing stage prompt.", data, t.TempDir())
		require.NoError(t, err)
		assert.Contains(t, out, "Existing stage prompt.\n\n## Workflow wake handoff")
		assert.Contains(t, out, "Wait ID: wait-1\nReason: ready\nWake count: 2")
		assert.Contains(t, out, "Output:\nCollected 雪\nsecond line")
		assert.Contains(t, out, "Continuation instructions:\nRead data.\nReport result.")
		assert.NotContains(t, out, "Release receipt:")
		assert.Equal(t, "ready", wake.Reason)
		assert.Equal(t, 2, wake.Count)
	}
	out, err := Render(`{{.Wake.Reason}}|{{.Wake.Output}}|{{.Wake.Count}}|{{.Wake.Next}}|{{.Wake.WaitID}}`, data, "")
	require.NoError(t, err)
	assert.Contains(t, out, "ready|Collected 雪\nsecond line|2|Read data.\nReport result.|wait-1")

	wake.Receipt = &ticket.WorkflowReceipt{ID: "receipt-1", Reason: "Approved", Commit: "abc123"}
	out, err = Render(`{{.Wake.Receipt.ID}}`, data, "")
	require.NoError(t, err)
	assert.Contains(t, out, "receipt-1\n\n## Workflow wake handoff")
	assert.Contains(t, out, "Release receipt:\n")
	assert.Contains(t, out, `"reason":"Approved"`)
	assert.Contains(t, out, `"commit":"abc123"`)

	wake.Reason, wake.Count, wake.Next = "released", 0, ""
	out, err = Render(`{{ with .Wake }}{{ .Reason }} {{ .Receipt.ID }}{{ end }}`, data, "")
	require.NoError(t, err)
	assert.Contains(t, out, "released receipt-1\n\n## Workflow wake handoff")
	assert.Contains(t, out, "Release receipt:\n")
	assert.NotContains(t, out, "Wake count:")
	assert.NotContains(t, out, "Continuation instructions:")

	out, err = Render("Unrelated node.", Data{}, "")
	require.NoError(t, err)
	assert.Equal(t, "Unrelated node.", out)
}

func TestFileOptional(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PLAN.md"), []byte("the plan\n雪"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "directory"), 0o700))
	for _, render := range []struct {
		name string
		fn   func(string, string) (string, error)
	}{
		{"ticket", func(tmpl, dir string) (string, error) { return Render(tmpl, Data{}, dir) }},
		{"automation", func(tmpl, dir string) (string, error) { return RenderAutomation(tmpl, AutomationData{}, dir) }},
	} {
		t.Run(render.name, func(t *testing.T) {
			out, err := render.fn(`[{{fileOptional "PLAN.md"}}]`, dir)
			require.NoError(t, err)
			assert.Equal(t, "[the plan\n雪]", out)
			out, err = render.fn(`[{{fileOptional "MISSING.md"}}]`, dir)
			require.NoError(t, err)
			assert.Equal(t, "[]", out)
			_, err = render.fn(`{{file "MISSING.md"}}`, dir)
			require.Error(t, err)
			_, err = render.fn(`{{fileOptional "directory"}}`, dir)
			require.Error(t, err)
			_, err = render.fn("{{fileOptional \"invalid\\x00path\"}}", dir)
			require.Error(t, err)
		})
	}
	_, err := CompileAutomation(`{{fileOptional "MISSING.md"}}`)
	require.NoError(t, err)
}
