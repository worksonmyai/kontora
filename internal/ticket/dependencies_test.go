package ticket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func dep(id string, status Status, deps ...string) *Ticket {
	return &Ticket{ID: id, Status: status, Kontora: true, Deps: deps}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name         string
		tickets      []*Ticket
		id           string
		wantReady    bool
		wantBlockers []string
		wantMissing  []string
	}{
		{
			name:      "no dependencies is ready",
			tickets:   []*Ticket{dep("a", StatusTodo)},
			id:        "a",
			wantReady: true,
		},
		{
			name:      "human_review dependency resolves",
			tickets:   []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusHumanReview)},
			id:        "a",
			wantReady: true,
		},
		{
			name:      "done dependency resolves",
			tickets:   []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusDone)},
			id:        "a",
			wantReady: true,
		},
		{
			name:      "cancelled dependency resolves",
			tickets:   []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusCancelled)},
			id:        "a",
			wantReady: true,
		},
		{
			name:      "archived dependency resolves",
			tickets:   []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusArchived)},
			id:        "a",
			wantReady: true,
		},
		{
			name:      "legacy closed dependency resolves",
			tickets:   []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusLegacyClosed)},
			id:        "a",
			wantReady: true,
		},
		{
			name:         "open dependency blocks",
			tickets:      []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusOpen)},
			id:           "a",
			wantBlockers: []string{"b"},
		},
		{
			name:         "todo dependency blocks",
			tickets:      []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusTodo)},
			id:           "a",
			wantBlockers: []string{"b"},
		},
		{
			name:         "in_progress dependency blocks",
			tickets:      []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusInProgress)},
			id:           "a",
			wantBlockers: []string{"b"},
		},
		{
			name:         "paused dependency blocks",
			tickets:      []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusPaused)},
			id:           "a",
			wantBlockers: []string{"b"},
		},
		{
			name:         "custom status dependency blocks",
			tickets:      []*Ticket{dep("a", StatusTodo, "b"), dep("b", "review")},
			id:           "a",
			wantBlockers: []string{"b"},
		},
		{
			name:         "missing dependency blocks and is reported missing",
			tickets:      []*Ticket{dep("a", StatusTodo, "ghost")},
			id:           "a",
			wantBlockers: []string{"ghost"},
			wantMissing:  []string{"ghost"},
		},
		{
			name:         "self dependency blocks",
			tickets:      []*Ticket{dep("a", StatusTodo, "a")},
			id:           "a",
			wantBlockers: []string{"a"},
		},
		{
			name: "every unresolved dependency is listed in order",
			tickets: []*Ticket{
				dep("a", StatusTodo, "c", "b", "ghost"),
				dep("b", StatusOpen),
				dep("c", StatusDone),
			},
			id:           "a",
			wantBlockers: []string{"b", "ghost"},
			wantMissing:  []string{"ghost"},
		},
		{
			name: "a ticket in a multi-node cycle is blocked",
			tickets: []*Ticket{
				dep("a", StatusTodo, "b"),
				dep("b", StatusTodo, "c"),
				dep("c", StatusTodo, "a"),
			},
			id:           "a",
			wantBlockers: []string{"b"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := Index(tc.tickets)
			got := Classify(index[tc.id], index)
			assert.Equal(t, tc.wantReady, got.Ready)
			assert.Equal(t, tc.wantBlockers, got.Blockers)
			assert.Equal(t, tc.wantMissing, got.Missing)
		})
	}
}

func TestDependencyPathAndCycle(t *testing.T) {
	cases := []struct {
		name      string
		tickets   []*Ticket
		from, to  string
		wantPath  []string
		cycleFor  string
		wantCycle []string
	}{
		{
			name:      "no path between unrelated tickets",
			tickets:   []*Ticket{dep("a", StatusTodo), dep("b", StatusTodo)},
			from:      "a",
			to:        "b",
			cycleFor:  "a",
			wantCycle: nil,
		},
		{
			name:      "direct edge",
			tickets:   []*Ticket{dep("a", StatusTodo, "b"), dep("b", StatusTodo)},
			from:      "a",
			to:        "b",
			wantPath:  []string{"a", "b"},
			cycleFor:  "a",
			wantCycle: nil,
		},
		{
			name: "transitive edge",
			tickets: []*Ticket{
				dep("a", StatusTodo, "b"),
				dep("b", StatusTodo, "c"),
				dep("c", StatusTodo),
			},
			from:     "a",
			to:       "c",
			wantPath: []string{"a", "b", "c"},
		},
		{
			name:      "same id is a one-element path",
			tickets:   []*Ticket{dep("a", StatusTodo)},
			from:      "a",
			to:        "a",
			wantPath:  []string{"a"},
			cycleFor:  "a",
			wantCycle: nil,
		},
		{
			name:      "self dependency is a cycle",
			tickets:   []*Ticket{dep("a", StatusTodo, "a")},
			cycleFor:  "a",
			wantCycle: []string{"a", "a"},
		},
		{
			name: "multi-node cycle",
			tickets: []*Ticket{
				dep("a", StatusTodo, "b"),
				dep("b", StatusTodo, "c"),
				dep("c", StatusTodo, "a"),
			},
			cycleFor:  "a",
			wantCycle: []string{"a", "b", "c", "a"},
		},
		{
			name:      "unknown ticket has no cycle",
			tickets:   []*Ticket{dep("a", StatusTodo)},
			cycleFor:  "ghost",
			wantCycle: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := Index(tc.tickets)
			if tc.from != "" {
				assert.Equal(t, tc.wantPath, DependencyPath(index, tc.from, tc.to))
			}
			if tc.cycleFor != "" {
				assert.Equal(t, tc.wantCycle, Cycle(index, tc.cycleFor))
			}
		})
	}
}

func TestDependents(t *testing.T) {
	index := Index([]*Ticket{
		dep("a", StatusTodo, "c"),
		dep("b", StatusTodo, "c"),
		dep("c", StatusTodo),
		dep("d", StatusTodo),
	})
	assert.Equal(t, []string{"a", "b"}, Dependents(index, "c"))
	assert.Nil(t, Dependents(index, "d"))
}
