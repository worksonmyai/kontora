package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestLocalExecutionActivityProjections(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		active       bool
	}{
		{"active", "status: awaiting_data\nrunning_status: awaiting_data\n", true},
		{"parked", "status: awaiting_data\n", false},
		{"inherited active", "status: awaiting_data\n<<: {running_status: awaiting_data}\n", true},
		{"stale", "status: awaiting_data\nrunning_status: other\n", false},
		{"passive epic", "status: in_progress\nkind: epic\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTicket(t, dir, "tst-001.md", "---\nid: tst-001\nkontora: true\n"+tc.fields+"started_at: 2026-01-01T00:00:00Z\n---\n# Searchable work\n")
			// The active snapshot must work after its declaration disappears.
			cfg := &config.Config{TicketsDir: dir}
			var list bytes.Buffer
			require.NoError(t, List(cfg, &list, ListOpts{JSON: true}))
			var items []ListItem
			require.NoError(t, json.Unmarshal(list.Bytes(), &items))
			require.Len(t, items, 1)
			assert.Equal(t, tc.active, items[0].ExecutionActive)
			assert.Contains(t, list.String(), `"execution_active":`)
			assert.Equal(t, items, FilterList(items, ListOpts{Status: items[0].Status}).Items)

			var found bytes.Buffer
			require.NoError(t, Search(cfg, &found, SearchOpts{Query: "Searchable", JSON: true}))
			var results []struct {
				ExecutionActive bool `json:"execution_active"`
			}
			require.NoError(t, json.Unmarshal(found.Bytes(), &results))
			require.Len(t, results, 1)
			assert.Equal(t, tc.active, results[0].ExecutionActive)
			assert.Contains(t, found.String(), `"execution_active":`)

			var view bytes.Buffer
			require.NoError(t, View(cfg, "tst-001", &view))
			assert.Contains(t, view.String(), "tst-001  "+items[0].Status)
			assert.Equal(t, tc.active, bytes.Contains(view.Bytes(), []byte("running:   ")))
		})
	}
}

func TestUpdateCustomActiveRejectsSpecificationChanges(t *testing.T) {
	for _, req := range []web.UpdateTicketRequest{
		{Body: new("replacement")}, {Description: new("replacement")},
		{Pipeline: new("one-stage")}, {Agent: new("a1")},
		{Branch: new("different")}, {BaseBranch: new("main")}, {Path: new("/other")},
	} {
		t.Run(fmt.Sprintf("%+v", req), func(t *testing.T) {
			dir := t.TempDir()
			original := "---\nid: tst-001\nstatus: blocked\nrunning_status: blocked\n---\n# Original\n"
			writeTicket(t, dir, "tst-001.md", original)
			require.ErrorIs(t, Update(updateTestConfig(dir), "tst-001", req), app.ErrInvalidState)
			out, err := os.ReadFile(filepath.Join(dir, "tst-001.md"))
			require.NoError(t, err)
			assert.Equal(t, original, string(out))
		})
	}
}
