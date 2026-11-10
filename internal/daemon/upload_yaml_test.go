package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/ticket"
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
