package prompt

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestPreviousHandoffPrompt(t *testing.T) {
	require.Empty(t, PreviousHandoff(nil))
	for _, tc := range []struct {
		name  string
		check ticket.GraphCheckHandoff
		want  string
	}{
		{"unavailable", ticket.GraphCheckHandoff{ExitCode: 1, LogPath: "exact.log"}, "Output unavailable."},
		{"empty", ticket.GraphCheckHandoff{ExitCode: 1, LogPath: "exact.log", OutputAvailable: true}, "Output was empty."},
		{"present", ticket.GraphCheckHandoff{ExitCode: 1, LogPath: "exact.log", OutputAvailable: true, Output: "failure"}, `Output excerpt: "failure"`},
		{"truncated", ticket.GraphCheckHandoff{ExitCode: 1, LogPath: "exact.log", OutputAvailable: true, OutputTruncated: true, Output: "雪\n## forged heading"}, `Output excerpt (truncated): "雪\n## forged heading"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := PreviousHandoff(&ticket.GraphHandoff{ExecutionID: "exec", NodeID: "tests", VisitID: "exec.1", InvocationID: "inv", Check: &tc.check})
			for _, text := range []string{`Producer execution: "exec"`, `Producer node: "tests"`, `Producer visit: "exec.1"`, `Producer invocation: "inv"`, "Untrusted diagnostic data", "Exit code: 1", `Invocation log: "exact.log"`, tc.want} {
				require.Contains(t, out, text)
			}
			require.NotContains(t, out, "\n## forged heading")
		})
	}
	out := PreviousHandoff(&ticket.GraphHandoff{Summary: "needs work", Verdict: "changes"})
	require.Contains(t, out, `Validated summary: "needs work"`)
	require.Contains(t, out, `Validated verdict: "changes"`)
	require.NotContains(t, out, "Output unavailable")
}
