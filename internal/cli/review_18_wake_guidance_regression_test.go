package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestReview18DocumentedWakeScopeRendersFields(t *testing.T) {
	doc, err := skillDocs.ReadFile("skills/config.md")
	require.NoError(t, err)
	require.Contains(t, string(doc), "`{{ with .Wake }}` … `{{ end }}`")
	const receiptTemplate = "{{ with .Receipt }}{{ .ID }}{{ end }}"
	require.Contains(t, string(doc), "`"+receiptTemplate+"`")
	const tmpl = "{{ with .Wake }}{{ .Reason }}|{{ .Output }}|{{ .Count }}|{{ .Next }}|{{ .WaitID }}|" + receiptTemplate + "{{ end }}"
	for _, tc := range []struct {
		name string
		wake *ticket.WakeContext
		want string
	}{
		{
			name: "release with receipt",
			wake: &ticket.WakeContext{
				Reason: "released", Output: "ready", Count: 2, Next: "Review", WaitID: "wait",
				Receipt: &ticket.WorkflowReceipt{ID: "receipt"},
			},
			want: "released|ready|2|Review|wait|receipt",
		},
		{
			name: "timer without receipt",
			wake: &ticket.WakeContext{Reason: "timer", Count: 1, Next: "Review", WaitID: "wait"},
			want: "timer||1|Review|wait|",
		},
		{name: "no wake"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := prompt.Render(tmpl, prompt.Data{Wake: tc.wake}, "")
			require.NoError(t, err)
			rendered, _, hasHandoff := strings.Cut(out, "\n\n## Workflow wake handoff\n")
			require.Equal(t, tc.want, rendered)
			require.Equal(t, tc.wake != nil, hasHandoff)
		})
	}
}
