package ticket

import (
	"testing"
	"time"

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

// child builds a ticket parented to an epic, created at a fixed offset so the
// created-order fallback in EpicChildren is deterministic.
func child(id string, status Status, parent string, createdMin int) *Ticket {
	at := time.Date(2026, 3, 1, 10, createdMin, 0, 0, time.UTC)
	return &Ticket{ID: id, Status: status, Kontora: true, Parent: parent, Created: &at}
}

func epic(id string, order ...string) *Ticket {
	return &Ticket{ID: id, Status: StatusOpen, Kontora: true, Kind: KindEpic, Children: order}
}

func TestDeriveEpicStatus(t *testing.T) {
	cases := []struct {
		name    string
		tickets []*Ticket
		want    Status
	}{
		{
			name:    "no children",
			tickets: []*Ticket{epic("e")},
			want:    StatusOpen,
		},
		{
			name:    "an open child holds the epic open",
			tickets: []*Ticket{epic("e"), child("a", StatusOpen, "e", 1), child("b", StatusDone, "e", 2)},
			want:    StatusOpen,
		},
		{
			name:    "a running child",
			tickets: []*Ticket{epic("e"), child("a", StatusOpen, "e", 1), child("b", StatusInProgress, "e", 2)},
			want:    StatusInProgress,
		},
		{
			name:    "a queued child",
			tickets: []*Ticket{epic("e"), child("a", StatusTodo, "e", 1)},
			want:    StatusInProgress,
		},
		{
			name:    "a child waiting on review",
			tickets: []*Ticket{epic("e"), child("a", StatusHumanReview, "e", 1)},
			want:    StatusInProgress,
		},
		{
			name:    "every child done",
			tickets: []*Ticket{epic("e"), child("a", StatusDone, "e", 1), child("b", StatusDone, "e", 2)},
			want:    StatusDone,
		},
		{
			name:    "every child cancelled",
			tickets: []*Ticket{epic("e"), child("a", StatusCancelled, "e", 1), child("b", StatusCancelled, "e", 2)},
			want:    StatusCancelled,
		},
		{
			name: "done and cancelled together close the epic",
			tickets: []*Ticket{
				epic("e"),
				child("a", StatusDone, "e", 1), child("b", StatusDone, "e", 2),
				child("c", StatusDone, "e", 3), child("d", StatusDone, "e", 4),
				child("f", StatusCancelled, "e", 5),
			},
			want: StatusDone,
		},
		{
			name:    "an archived child is out of the meter",
			tickets: []*Ticket{epic("e"), child("a", StatusDone, "e", 1), child("b", StatusArchived, "e", 2)},
			want:    StatusDone,
		},
		{
			name:    "only archived children reads as no children",
			tickets: []*Ticket{epic("e"), child("a", StatusArchived, "e", 1)},
			want:    StatusOpen,
		},
		{
			name:    "a ticket parented elsewhere is not a child",
			tickets: []*Ticket{epic("e"), child("a", StatusInProgress, "other", 1)},
			want:    StatusOpen,
		},
		{
			name:    "an unknown epic",
			tickets: []*Ticket{child("a", StatusDone, "e", 1)},
			want:    StatusOpen,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DeriveEpicStatus(Index(tc.tickets), "e"))
		})
	}
}

func TestEpicChildren(t *testing.T) {
	cases := []struct {
		name    string
		tickets []*Ticket
		want    []string
	}{
		{
			name:    "the children list sets the order",
			tickets: []*Ticket{epic("e", "c", "a", "b"), child("a", StatusOpen, "e", 1), child("b", StatusOpen, "e", 2), child("c", StatusOpen, "e", 3)},
			want:    []string{"c", "a", "b"},
		},
		{
			name:    "a child the list misses sorts last by created",
			tickets: []*Ticket{epic("e", "c", "a"), child("a", StatusOpen, "e", 3), child("b", StatusOpen, "e", 2), child("c", StatusOpen, "e", 9), child("d", StatusOpen, "e", 1)},
			want:    []string{"c", "a", "d", "b"},
		},
		{
			name:    "no list at all is created order",
			tickets: []*Ticket{epic("e"), child("a", StatusOpen, "e", 3), child("b", StatusOpen, "e", 1)},
			want:    []string{"b", "a"},
		},
		{
			name:    "an id in the list that names no child is ignored",
			tickets: []*Ticket{epic("e", "ghost", "a"), child("a", StatusOpen, "e", 1)},
			want:    []string{"a"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range EpicChildren(Index(tc.tickets), "e") {
				got = append(got, c.ID)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
