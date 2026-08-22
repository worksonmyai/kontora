package ticket

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// designGraph is the six-node graph the chain ladder was designed against:
// three deps feed u8ax, which feeds the open ticket, which feeds the goal.
func designGraph() []*Ticket {
	return []*Ticket{
		dep("pca-itf5", StatusDone),
		dep("pca-csyi", StatusDone),
		dep("pca-qv0y", StatusPaused),
		dep("pca-u8ax", StatusOpen, "pca-itf5", "pca-csyi", "pca-qv0y"),
		dep("pca-d3q9", StatusTodo, "pca-u8ax"),
		dep("pca-uvby", StatusOpen, "pca-d3q9"),
	}
}

func chainOf(tickets []*Ticket, id string, limit int) ChainResult {
	return Chain(Index(tickets), id, limit)
}

func nodeIDs(r ChainResult) []string {
	ids := make([]string, len(r.Nodes))
	for i, n := range r.Nodes {
		ids[i] = n.ID
	}
	return ids
}

func nodeDepths(r ChainResult) []int {
	ds := make([]int, len(r.Nodes))
	for i, n := range r.Nodes {
		ds[i] = n.Depth
	}
	return ds
}

func nodeByID(r ChainResult, id string) (ChainNode, bool) {
	for _, n := range r.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return ChainNode{}, false
}

func nodeDirections(r ChainResult) []string {
	ds := make([]string, len(r.Nodes))
	for i, n := range r.Nodes {
		ds[i] = n.Direction
	}
	return ds
}

func TestChain(t *testing.T) {
	cases := []struct {
		name           string
		tickets        []*Ticket
		id             string
		wantVerdict    string
		wantIDs        []string
		wantDepths     []int
		wantDirections []string
		wantPosition   int
		wantPathLength int
		wantGoal       string
		wantTotal      int
		wantDone       int
		wantHolder     string
		wantCritical   []string
		wantWaits      map[string][2]int
		wantCycle      []string
	}{
		{
			name:           "the design's six-node graph",
			tickets:        designGraph(),
			id:             "pca-d3q9",
			wantVerdict:    ChainVerdictBlocked,
			wantIDs:        []string{"pca-itf5", "pca-csyi", "pca-qv0y", "pca-u8ax", "pca-d3q9", "pca-uvby"},
			wantDepths:     []int{0, 0, 0, 1, 2, 3},
			wantDirections: []string{ChainUpstream, ChainUpstream, ChainUpstream, ChainUpstream, ChainSelf, ChainDownstream},
			wantPosition:   3,
			wantPathLength: 4,
			wantGoal:       "pca-uvby",
			wantTotal:      6,
			wantDone:       2,
			wantHolder:     "pca-qv0y",
			// The paused dep, not either done one: a resolved dep contributes
			// nothing to when the goal can start.
			wantCritical: []string{"pca-qv0y", "pca-u8ax", "pca-d3q9", "pca-uvby"},
			// Every dep counts, not only the ones in the chain.
			wantWaits: map[string][2]int{"pca-u8ax": {1, 3}, "pca-d3q9": {1, 1}, "pca-qv0y": {0, 0}},
		},
		{
			name: "a diamond puts both middles on one layer",
			tickets: []*Ticket{
				dep("root", StatusOpen),
				dep("left", StatusOpen, "root"),
				dep("right", StatusOpen, "root"),
				dep("self", StatusTodo, "left", "right"),
			},
			id:             "self",
			wantVerdict:    ChainVerdictBlocked,
			wantIDs:        []string{"root", "left", "right", "self"},
			wantDepths:     []int{0, 1, 1, 2},
			wantDirections: []string{ChainUpstream, ChainUpstream, ChainUpstream, ChainSelf},
			wantPosition:   3,
			wantPathLength: 3,
			wantGoal:       "self",
			wantTotal:      4,
			wantHolder:     "root",
			wantCritical:   []string{"root", "left", "self"},
		},
		{
			// The forward walk ranks on height, so it takes the branch with the
			// most chain left ahead of it and not the deepest dependent.
			name: "branching dependents pick the longer branch",
			tickets: []*Ticket{
				dep("self", StatusTodo),
				dep("aa", StatusOpen, "self"),
				dep("zz", StatusOpen, "self"),
				dep("zz2", StatusOpen, "zz"),
			},
			id:             "self",
			wantVerdict:    ChainVerdictReady,
			wantIDs:        []string{"self", "aa", "zz", "zz2"},
			wantDepths:     []int{0, 1, 1, 2},
			wantDirections: []string{ChainSelf, ChainDownstream, ChainDownstream, ChainDownstream},
			wantPosition:   1,
			wantPathLength: 3,
			wantGoal:       "zz2",
			wantTotal:      4,
			wantHolder:     "self",
			wantCritical:   []string{"self", "zz", "zz2"},
		},
		{
			// The open dep of a done dep is on the critical path and blocks
			// nothing: the ticket is schedulable now, so no upstream node holds
			// the chain.
			name: "an unresolved node behind a done dep does not hold the chain",
			tickets: []*Ticket{
				dep("e", StatusOpen),
				dep("d", StatusDone, "e"),
				dep("self", StatusTodo, "d"),
			},
			id:             "self",
			wantVerdict:    ChainVerdictReady,
			wantIDs:        []string{"e", "d", "self"},
			wantDepths:     []int{0, 1, 2},
			wantDirections: []string{ChainUpstream, ChainUpstream, ChainSelf},
			wantPosition:   3,
			wantPathLength: 3,
			wantGoal:       "self",
			wantTotal:      3,
			wantDone:       1,
			wantHolder:     "self",
			wantCritical:   []string{"e", "d", "self"},
		},
		{
			// The blocker is off the critical path: a longer resolved chain
			// wins the path, and the one open dep is still what work waits on.
			name: "the holder is the blocker, not the first unresolved node on the path",
			tickets: []*Ticket{
				dep("r1", StatusDone),
				dep("r2", StatusDone, "r1"),
				dep("r3", StatusDone, "r2"),
				dep("b", StatusOpen),
				dep("self", StatusTodo, "r3", "b"),
			},
			id:             "self",
			wantVerdict:    ChainVerdictBlocked,
			wantIDs:        []string{"b", "r1", "r2", "r3", "self"},
			wantDepths:     []int{0, 0, 1, 2, 3},
			wantDirections: []string{ChainUpstream, ChainUpstream, ChainUpstream, ChainUpstream, ChainSelf},
			wantPosition:   4,
			wantPathLength: 4,
			wantGoal:       "self",
			wantTotal:      5,
			wantDone:       3,
			wantHolder:     "b",
			wantCritical:   []string{"r1", "r2", "r3", "self"},
		},
		{
			// Two hops of blockers: the holder is the one work can start on,
			// not the dep the ticket names.
			name: "the holder is the deepest blocker work can start on",
			tickets: []*Ticket{
				dep("e", StatusOpen),
				dep("d", StatusOpen, "e"),
				dep("self", StatusTodo, "d"),
			},
			id:             "self",
			wantVerdict:    ChainVerdictBlocked,
			wantIDs:        []string{"e", "d", "self"},
			wantDepths:     []int{0, 1, 2},
			wantDirections: []string{ChainUpstream, ChainUpstream, ChainSelf},
			wantPosition:   3,
			wantPathLength: 3,
			wantGoal:       "self",
			wantTotal:      3,
			wantHolder:     "e",
			wantCritical:   []string{"e", "d", "self"},
		},
		{
			name: "a cycle through the ticket",
			tickets: []*Ticket{
				dep("pca-d3q9", StatusTodo, "pca-u8ax"),
				dep("pca-u8ax", StatusOpen, "pca-d3q9"),
			},
			id:          "pca-d3q9",
			wantVerdict: ChainVerdictCycle,
			wantIDs:     []string{},
			wantCycle:   []string{"pca-d3q9", "pca-u8ax", "pca-d3q9"},
		},
		{
			name: "a cycle downstream of the ticket",
			tickets: []*Ticket{
				dep("root", StatusDone),
				dep("a", StatusOpen, "root", "b"),
				dep("b", StatusOpen, "a"),
			},
			id:          "root",
			wantVerdict: ChainVerdictCycle,
			wantIDs:     []string{},
			wantCycle:   []string{"a", "b", "a"},
		},
		{
			name:        "a ticket that depends on itself",
			tickets:     []*Ticket{dep("self", StatusTodo, "self")},
			id:          "self",
			wantVerdict: ChainVerdictCycle,
			wantIDs:     []string{},
			wantCycle:   []string{"self", "self"},
		},
		{
			name: "a dep with no ticket file",
			tickets: []*Ticket{
				dep("self", StatusTodo, "gone"),
			},
			id:             "self",
			wantVerdict:    ChainVerdictBlocked,
			wantIDs:        []string{"gone", "self"},
			wantDepths:     []int{0, 1},
			wantDirections: []string{ChainUpstream, ChainSelf},
			wantPosition:   2,
			wantPathLength: 2,
			wantGoal:       "self",
			wantTotal:      2,
			wantHolder:     "gone",
		},
		{
			name:           "a ticket with no relations is its own chain",
			tickets:        []*Ticket{dep("self", StatusTodo)},
			id:             "self",
			wantVerdict:    ChainVerdictReady,
			wantIDs:        []string{"self"},
			wantDepths:     []int{0},
			wantDirections: []string{ChainSelf},
			wantPosition:   1,
			wantPathLength: 1,
			wantGoal:       "self",
			wantTotal:      1,
			wantHolder:     "self",
		},
		{
			name:           "closed deps report ready even when the ticket is done",
			tickets:        []*Ticket{dep("self", StatusDone, "a"), dep("a", StatusCancelled)},
			id:             "self",
			wantVerdict:    ChainVerdictReady,
			wantIDs:        []string{"a", "self"},
			wantDepths:     []int{0, 1},
			wantDirections: []string{ChainUpstream, ChainSelf},
			wantPosition:   2,
			wantPathLength: 2,
			wantGoal:       "self",
			wantTotal:      2,
			wantDone:       2,
		},
		{
			name:        "an id with no ticket returns an empty chain",
			tickets:     []*Ticket{dep("self", StatusTodo)},
			id:          "nope",
			wantVerdict: ChainVerdictReady,
			wantIDs:     []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chainOf(tc.tickets, tc.id, 0)

			assert.Equal(t, tc.id, got.ID)
			assert.Equal(t, tc.wantVerdict, got.Verdict)
			assert.Equal(t, tc.wantIDs, nodeIDs(got))
			assert.Equal(t, tc.wantPosition, got.Position)
			assert.Equal(t, tc.wantPathLength, got.PathLength)
			assert.Equal(t, tc.wantGoal, got.Goal)
			assert.Equal(t, tc.wantTotal, got.Total)
			assert.Equal(t, tc.wantDone, got.Done)

			if tc.wantCycle != nil {
				assert.Equal(t, tc.wantCycle, got.Cycle)
				assert.Empty(t, got.Nodes, "a cycle reports no ladder")
			}
			if tc.wantDepths != nil {
				assert.Equal(t, tc.wantDepths, nodeDepths(got))
			}
			if tc.wantDirections != nil {
				assert.Equal(t, tc.wantDirections, nodeDirections(got))
			}

			var holders []string
			for _, n := range got.Nodes {
				if n.HoldsChain {
					holders = append(holders, n.ID)
				}
			}
			if tc.wantHolder == "" {
				assert.Empty(t, holders)
			} else {
				assert.Equal(t, []string{tc.wantHolder}, holders)
			}

			if tc.wantCritical != nil {
				var onPath []string
				for _, n := range got.Nodes {
					if n.OnCriticalPath {
						onPath = append(onPath, n.ID)
					}
				}
				assert.Equal(t, tc.wantCritical, onPath)
			}
			for id, want := range tc.wantWaits {
				n, ok := nodeByID(got, id)
				require.True(t, ok, id)
				assert.Equal(t, want, [2]int{n.WaitsOpen, n.WaitsTotal}, id)
			}

			// Depth never goes backwards, so the rows read roots to goal.
			for i := 1; i < len(got.Nodes); i++ {
				assert.GreaterOrEqual(t, got.Nodes[i].Depth, got.Nodes[i-1].Depth)
			}
		})
	}
}

func TestChainCap(t *testing.T) {
	// One long path out of self, plus a wide fan of leaf deps to trim.
	tickets := []*Ticket{dep("self", StatusTodo, "chain-0")}
	for i := range 5 {
		tickets = append(tickets, dep(fmt.Sprintf("chain-%d", i), StatusOpen, fmt.Sprintf("chain-%d", i+1)))
	}
	tickets = append(tickets, dep("chain-5", StatusOpen))
	for i := range 20 {
		id := fmt.Sprintf("leaf-%02d", i)
		tickets = append(tickets, dep(id, StatusOpen))
		tickets[0].Deps = append(tickets[0].Deps, id)
	}

	got := chainOf(tickets, "self", 10)

	assert.Equal(t, ChainVerdictBlocked, got.Verdict)
	assert.Equal(t, 27, got.Total, "total is the true count, not the capped one")
	require.Len(t, got.Nodes, 10)

	kept := map[string]bool{}
	for _, n := range got.Nodes {
		kept[n.ID] = true
	}
	for i := range 6 {
		assert.True(t, kept[fmt.Sprintf("chain-%d", i)], "the critical path survives the cap")
	}
	assert.True(t, kept["self"])
	for i := 1; i < len(got.Nodes); i++ {
		assert.GreaterOrEqual(t, got.Nodes[i].Depth, got.Nodes[i-1].Depth)
	}
}

// The holder is not always on the critical path, so the cap has to keep it too:
// a trimmed chain that says nothing about what is blocking it is the finding
// the cap exists to avoid.
func TestChainCapKeepsTheHolder(t *testing.T) {
	// A resolved three-node chain wins the path; the one open dep, named last,
	// sorts behind the resolved siblings and would be the first thing trimmed.
	self := dep("self", StatusTodo, "done-00")
	tickets := []*Ticket{
		dep("d2", StatusDone),
		dep("d1", StatusDone, "d2"),
		dep("done-00", StatusDone, "d1"),
		dep("blk", StatusOpen),
		self,
	}
	for i := 1; i < 20; i++ {
		id := fmt.Sprintf("done-%02d", i)
		tickets = append(tickets, dep(id, StatusDone))
		self.Deps = append(self.Deps, id)
	}
	self.Deps = append(self.Deps, "blk")

	got := chainOf(tickets, "self", 6)

	require.Len(t, got.Nodes, 6)
	holder, ok := nodeByID(got, "blk")
	require.True(t, ok, "the holder survives the cap")
	assert.True(t, holder.HoldsChain)
	assert.False(t, holder.OnCriticalPath)
}
