package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/store"
)

func TestUploadRejectsShadowedAliasExpansion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
		leaf  string
	}{
		{name: "depth_12", depth: 12, leaf: "leaf"},
		{name: "depth_40", depth: 40, leaf: "leaf"},
		{name: "repeated_text", depth: 7, leaf: strings.Repeat("x", 64<<10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fm strings.Builder
			fmt.Fprintf(&fm, "id: imported\nstatus: open\npayload: safe\n<<:\n  - &n0 {payload: %s}\n", tc.leaf)
			for i := 1; i <= tc.depth; i++ {
				fmt.Fprintf(&fm, "  - &n%d {payload: [*n%d, *n%d]}\n", i, i-1, i-1)
			}
			var root yaml.Node
			require.NoError(t, yaml.Unmarshal([]byte(fm.String()), &root))
			var decoded map[string]any
			// The decoder skips inherited payloads because the root already defines payload.
			require.NoError(t, root.Decode(&decoded))
			require.Equal(t, "safe", decoded["payload"])

			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			before, err := filepath.Glob(filepath.Join(h.tasksDir, "*.md"))
			require.NoError(t, err)
			_, err = d.UploadTicket([]byte("---\n" + fm.String() + "---\n# Imported\n"))
			require.ErrorContains(t, err, "YAML expansion limit")
			after, err := filepath.Glob(filepath.Join(h.tasksDir, "*.md"))
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Empty(t, d.tickets)
			assert.Empty(t, d.queuedIDs())
		})
	}
}

func TestUploadAndTransferStripPredecessorHandoff(t *testing.T) {
	const input = `---
id: tst-handoff
status: paused
stage: repair
graph_state:
  version: 1
  execution_id: execution
  revision: 8
  definition_fingerprint: definition
  visit: 2
  visit_id: execution.2
  handoff:
    execution_id: execution
    node_id: tests
    visit_id: execution.1
    invocation_id: check-1
    destination_visit_id: execution.2
    check:
      exit_code: 1
      log_path: exact.log
      output: local diagnostic bytes
      output_available: true
---
# Imported
`
	anchored := strings.Replace(input, "output: local diagnostic", "output: &diagnostic local diagnostic", 1)
	plainFrontmatter, body, _ := strings.Cut(input[4:], "\n---\n")
	beforeState, state, _ := strings.Cut(plainFrontmatter, "graph_state:\n")
	merged := "---\n" + beforeState + "<<:\n  graph_state:\n    " + strings.ReplaceAll(state, "\n", "\n    ") + "\n---\n" + body
	for _, tc := range []struct {
		name  string
		data  string
		alias bool
	}{
		{name: "plain", data: input},
		{name: "inline root merge", data: merged},
		{name: "alias to output", data: strings.Replace(anchored, "\n---\n", "\ncustom: [*diagnostic]\n---\n", 1), alias: true},
		{name: "root merge copies output", data: strings.Replace(anchored, "\n---\n", "\n<<: {custom: *diagnostic}\n---\n", 1), alias: true},
		{name: "alias to graph state", data: strings.Replace(strings.Replace(input, "graph_state:", "graph_state: &live", 1), "\n---\n", "\ncustom: *live\n---\n", 1), alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ticket.ParseBytes([]byte(tc.data))
			require.NoError(t, err)
			require.Equal(t, "local diagnostic bytes", parsed.GraphState.Handoff.Check.Output)
			assertStripped := func(path string) {
				t.Helper()
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NotContains(t, string(data), "graph_state:")
				require.NotContains(t, string(data), "local diagnostic bytes")
			}
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			info, uploadErr := d.UploadTicket([]byte(tc.data))
			src, dst := t.TempDir(), t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(src, parsed.ID+".md"), []byte(tc.data), 0o600))
			rev, err := store.Revision(src, parsed.ID)
			require.NoError(t, err)
			moved, transferErr := store.Transfer(store.TransferRequest{ID: parsed.ID, SourceDir: src, DestinationDir: dst, Revision: rev})
			if tc.alias {
				require.ErrorContains(t, uploadErr, "graph_state shares YAML nodes")
				require.ErrorContains(t, transferErr, "graph_state shares YAML nodes")
				require.Empty(t, d.tickets)
				require.NoFileExists(t, filepath.Join(dst, parsed.ID+".md"))
				return
			}
			require.NoError(t, uploadErr)
			require.NoError(t, transferErr)
			assertStripped(filepath.Join(h.tasksDir, info.ID+".md"))
			assertStripped(moved.FilePath)
		})
	}
}

func TestUploadAliasBudgetBeforeAllocation(t *testing.T) {
	scalar := &yaml.Node{Kind: yaml.ScalarNode, Value: "value"}
	for _, tc := range []struct {
		name   string
		node   *yaml.Node
		budget uploadYAMLBudget
		wantOK bool
	}{
		{
			name:   "exact budget",
			node:   &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar, scalar}},
			budget: uploadYAMLBudget{nodes: 3, slots: 2, textBytes: 10},
			wantOK: true,
		},
		{
			name:   "nodes shared across siblings",
			node:   &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar, scalar}},
			budget: uploadYAMLBudget{nodes: 2, slots: 2, textBytes: 10},
		},
		{
			name:   "text shared across siblings",
			node:   &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar, scalar}},
			budget: uploadYAMLBudget{nodes: 3, slots: 2, textBytes: 9},
		},
		{
			name:   "comments count as text",
			node:   &yaml.Node{Kind: yaml.ScalarNode, HeadComment: "123456"},
			budget: uploadYAMLBudget{nodes: 1, textBytes: 5},
		},
		{
			name: "slice rejected before visiting children",
			// Nil children would panic if expansion started before reserving the slice.
			node:   &yaml.Node{Kind: yaml.SequenceNode, Content: make([]*yaml.Node, 100_000)},
			budget: uploadYAMLBudget{nodes: 10, slots: 10, textBytes: 10},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := expandUploadAliases(tc.node, make(map[*yaml.Node]bool), &tc.budget)
			if tc.wantOK {
				require.NoError(t, err)
				require.Len(t, out.Content, 2)
				assert.NotSame(t, out.Content[0], out.Content[1])
			} else {
				require.ErrorContains(t, err, "YAML expansion limit")
				assert.Nil(t, out)
			}
		})
	}
}

func TestUploadAliasBudgetRejectsLargeSliceWithoutAllocating(t *testing.T) {
	node := &yaml.Node{Kind: yaml.SequenceNode, Content: make([]*yaml.Node, 100_000)}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			budget := uploadYAMLBudget{nodes: 10, slots: 10, textBytes: 10}
			if _, err := expandUploadAliases(node, make(map[*yaml.Node]bool), &budget); err == nil {
				b.Fatal("expected expansion limit")
			}
		}
	})
	// An early rejection must not allocate the 100,000-element slice.
	assert.Less(t, result.AllocedBytesPerOp(), int64(1024))
}

func TestUploadRejectsShadowedAliasCycle(t *testing.T) {
	_, err := materializeUploadMerges([]byte("---\npayload: safe\n<<: &loop {payload: *loop}\n---\n# Imported\n"))
	require.ErrorContains(t, err, "cyclic YAML alias")
}

func TestUploadMaterializesSmallAliasGraph(t *testing.T) {
	h := newHarness(t)
	d := h.newDaemon(h.cfg)
	content := `---
id: imported
status: todo
scheduled_at: "2021-01-01T00:00:00Z"
<<:
  - &defaults
    scheduled_at: "2020-01-01T00:00:00Z"
    custom: &values ["001", true, 7]
copy: *values
---
# Imported
`
	info, err := d.UploadTicket([]byte(content))
	require.NoError(t, err)
	assert.Equal(t, "open", info.Status)
	assert.Empty(t, info.ScheduledAt)
	got := h.readTask(info.ID + ".md")
	assert.Equal(t, ticket.StatusOpen, got.Status)
	assert.Empty(t, got.ScheduledAt)
	data, err := got.Marshal()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "scheduled_at")
	assert.NotContains(t, string(data), "<<:")
	assert.NotContains(t, string(data), "*values")
	assert.Contains(t, string(data), `"001"`)
	d.promoteDueSchedules(time.Now())
	assert.Empty(t, d.queuedIDs())
}
