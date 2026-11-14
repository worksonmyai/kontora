package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestTransferRefusesWorkflowWaitAuthority(t *testing.T) {
	for _, fields := range []string{
		"phase: pending\n    disarmed: false",
		"phase: pending\n    disarmed: true",
		"phase: ready\n    disarmed: false",
		"phase: recovery_required\n    disarmed: true",
	} {
		t.Run(strings.Fields(fields)[1], func(t *testing.T) {
			req := transferFixture(t, "graph_state:\n  version: 2\n  execution_id: execution\n  revision: 7\n  definition_fingerprint: definition\n  wait:\n    id: wait-1\n    "+fields+"\n", true)
			source := filepath.Join(req.SourceDir, req.ID+".md")
			before, err := os.ReadFile(source)
			require.NoError(t, err)
			validated := false
			req.Validate = func(_ *ticket.Ticket) error { validated = true; return nil }
			_, err = Transfer(req)
			require.ErrorContains(t, err, "active workflow wait")
			assert.False(t, validated, "storage must enforce wait authority even without daemon validation")
			after, err := os.ReadFile(source)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			require.NoFileExists(t, filepath.Join(req.DestinationDir, req.ID+".md"))
			require.FileExists(t, filepath.Join(req.SourceDir, req.ID+".notes.json"))
		})
	}
}
