package cli

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestReview18DocumentedWakeScopeRendersFields(t *testing.T) {
	doc, err := skillDocs.ReadFile("skills/config.md")
	require.NoError(t, err)
	require.Contains(t, string(doc), "`{{ with .Wake }}` … `{{ end }}`")
	tmpl := "{{ with .Wake }}{{ .Reason }}|{{ .Output }}|{{ .Count }}|{{ .Next }}|{{ .WaitID }}|{{ .Receipt.ID }}{{ end }}"
	out, err := prompt.Render(tmpl, prompt.Data{Wake: &ticket.WakeContext{
		Reason: "released", Output: "ready", Count: 2, Next: "Review", WaitID: "wait",
		Receipt: &ticket.WorkflowReceipt{ID: "receipt"},
	}}, "")
	require.NoError(t, err)
	require.Contains(t, out, "released|ready|2|Review|wait|receipt")
	out, err = prompt.Render(tmpl, prompt.Data{}, "")
	require.NoError(t, err)
	require.Empty(t, out)
}
