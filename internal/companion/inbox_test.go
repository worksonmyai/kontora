package companion

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var observedTime = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)

func observation(source, condition string) Observation {
	return Observation{Source: source, TicketID: "kon-test", Condition: condition, Revision: "run-1", Title: "Needs attention", Detail: "Recorded detail"}
}

func TestReconcile(t *testing.T) {
	paused := observation("ticket-created-1/run-1", "paused")
	questionA := observation("invocation-1/tool-A", "question")
	questionB := observation("invocation-1/tool-B", "question")
	completed := observation("ticket-created-1/run-1", "completed")
	type wantItem struct {
		occurrence int
		active     bool
		read       bool
		reason     string
	}
	type step struct {
		observations []Observation
		startup      bool
		read         []int
		changed      bool
		items        []wantItem
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "continuing status retains identity and read state",
			steps: []step{
				{observations: []Observation{paused}, startup: true, changed: true, read: []int{0}, items: []wantItem{{1, true, true, ""}}},
				{observations: []Observation{paused}, items: []wantItem{{1, true, true, ""}}},
				{observations: []Observation{paused}, startup: true, items: []wantItem{{1, true, true, ""}}},
			},
		},
		{
			name: "status exit and reentry create an unread occurrence",
			steps: []step{
				{observations: []Observation{paused}, changed: true, read: []int{0}, items: []wantItem{{1, true, true, ""}}},
				{changed: true, items: []wantItem{{1, false, true, "no_longer_observed"}}},
				{observations: []Observation{paused}, changed: true, items: []wantItem{{1, false, true, "no_longer_observed"}, {2, true, false, ""}}},
			},
		},
		{
			name: "question A B A restores the original read state",
			steps: []step{
				{observations: []Observation{questionA}, changed: true, read: []int{0}, items: []wantItem{{1, true, true, ""}}},
				{observations: []Observation{questionB}, changed: true, items: []wantItem{{1, false, true, "no_longer_observed"}, {2, true, false, ""}}},
				{observations: []Observation{questionA}, changed: true, items: []wantItem{{1, true, true, ""}, {2, false, false, "no_longer_observed"}}},
			},
		},
		{
			name: "question disappearance does not prove an answer",
			steps: []step{
				{observations: []Observation{questionA}, changed: true, items: []wantItem{{1, true, false, ""}}},
				{changed: true, items: []wantItem{{1, false, false, "no_longer_observed"}}},
				{items: []wantItem{{1, false, false, "no_longer_observed"}}},
			},
		},
		{
			name: "startup interrupts old questions even if the source is reused",
			steps: []step{
				{observations: []Observation{questionA, paused}, changed: true, read: []int{0, 1}, items: []wantItem{{1, true, true, ""}, {2, true, true, ""}}},
				{observations: []Observation{questionA, paused}, startup: true, changed: true, items: []wantItem{{1, false, true, "interrupted"}, {2, true, true, ""}, {3, true, false, ""}}},
				{observations: []Observation{questionA, paused}, items: []wantItem{{1, false, true, "interrupted"}, {2, true, true, ""}, {3, true, false, ""}}},
			},
		},
		{
			name: "completion is inactive history and survives observation gaps",
			steps: []step{
				{observations: []Observation{paused}, changed: true, items: []wantItem{{1, true, false, ""}}},
				{observations: []Observation{completed}, changed: true, items: []wantItem{{1, false, false, "no_longer_observed"}, {2, false, false, "completed"}}},
				{items: []wantItem{{1, false, false, "no_longer_observed"}, {2, false, false, "completed"}}},
				{observations: []Observation{completed}, items: []wantItem{{1, false, false, "no_longer_observed"}, {2, false, false, "completed"}}},
			},
		},
		{
			name: "initial backlog includes completions without active attention",
			steps: []step{
				{observations: []Observation{paused, completed}, startup: true, changed: true, items: []wantItem{{1, true, false, ""}, {2, false, false, "completed"}}},
			},
		},
		{
			name: "ticket recreation changes source identity",
			steps: []step{
				{observations: []Observation{paused}, changed: true, read: []int{0}, items: []wantItem{{1, true, true, ""}}},
				{observations: []Observation{observation("ticket-created-2/run-1", "paused")}, changed: true, items: []wantItem{{1, false, true, "no_longer_observed"}, {2, true, false, ""}}},
			},
		},
		{
			name: "empty startup is initialized once",
			steps: []step{
				{startup: true, changed: true},
				{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			ids := map[int]string{}
			firstSeen := map[int]time.Time{}
			for n, step := range tt.steps {
				now := observedTime.Add(time.Duration(n) * time.Minute)
				before := state.Revision
				require.Equal(t, step.changed, state.Reconcile(step.observations, now, step.startup), "step %d", n)
				if step.changed {
					require.Equal(t, before+1, state.Revision)
				} else {
					require.Equal(t, before, state.Revision)
				}
				for _, index := range step.read {
					item := state.Items[index]
					require.NoError(t, state.SetRead(item.ID, item.Revision, true))
				}
				require.Equal(t, snapshotVersion, state.Version)
				require.Equal(t, now, state.ObservedAt)
				require.Len(t, state.Items, len(step.items), "step %d", n)
				for i, want := range step.items {
					item := state.Items[i]
					if id, ok := ids[want.occurrence]; ok {
						require.Equal(t, id, item.ID)
						require.Equal(t, firstSeen[want.occurrence], item.FirstSeen)
					} else {
						for _, id := range ids {
							require.NotEqual(t, id, item.ID)
						}
						ids[want.occurrence] = item.ID
						firstSeen[want.occurrence] = item.FirstSeen
					}
					require.Equal(t, want.active, item.Active)
					require.Equal(t, want.read, item.Read)
					require.Equal(t, want.reason, item.InactiveReason)
					require.LessOrEqual(t, item.Revision, state.Revision)
				}
			}
		})
	}
}

func TestObservationChanges(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Observation)
	}{
		{"source revision", func(o *Observation) { o.Revision = "run-2" }},
		{"title", func(o *Observation) { o.Title = "Updated title" }},
		{"detail", func(o *Observation) { o.Detail = "Updated detail" }},
		{"missing question identity", func(o *Observation) { o.IdentityLimited = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			o := observation("invocation/tool", "question")
			require.True(t, state.Reconcile([]Observation{o}, observedTime, false))
			require.NoError(t, state.SetRead(state.Items[0].ID, state.Items[0].Revision, true))
			original := state.Items[0]
			tt.change(&o)
			now := observedTime.Add(time.Minute)
			require.True(t, state.Reconcile([]Observation{o}, now, false))
			require.Len(t, state.Items, 1)
			item := state.Items[0]
			require.Equal(t, original.ID, item.ID)
			require.Equal(t, original.FirstSeen, item.FirstSeen)
			require.Equal(t, now, item.ChangedAt)
			require.Equal(t, state.Revision, item.ChangeRevision)
			require.True(t, item.Read)
			require.Equal(t, o.Revision, item.SourceRevision)
			require.Equal(t, o.Title, item.Title)
			require.Equal(t, o.Detail, item.Detail)
			require.Equal(t, o.IdentityLimited, item.IdentityLimited)
			require.False(t, state.Reconcile([]Observation{o, o}, now.Add(time.Minute), false))
		})
	}
}

func TestSetRead(t *testing.T) {
	for _, tt := range []struct {
		name     string
		id       string
		revision uint64
		read     bool
		wantErr  error
		changed  bool
	}{
		{"read", "", 1, true, nil, true},
		{"already unread", "", 1, false, nil, false},
		{"stale revision", "", 0, true, ErrConflict, false},
		{"future revision", "", 2, true, ErrConflict, false},
		{"missing item", "missing", 1, true, ErrNotFound, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			state.Reconcile([]Observation{observation("status", "paused"), observation("question", "question")}, observedTime, false)
			before := cloneState(t, state)
			id := tt.id
			if id == "" {
				id = state.Items[0].ID
			}
			err := state.SetRead(id, tt.revision, tt.read)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tt.changed {
				require.Equal(t, before.Revision+1, state.Revision)
				require.Equal(t, state.Revision, state.Items[0].Revision)
				require.True(t, state.Items[0].Read)
				require.True(t, state.Items[0].Active)
				require.Equal(t, before.Items[0].ChangedAt, state.Items[0].ChangedAt)
				require.Equal(t, before.Items[0].ChangeRevision, state.Items[0].ChangeRevision)
				require.Equal(t, before.Items[1], state.Items[1])
				require.ErrorIs(t, state.SetRead(id, tt.revision, true), ErrConflict)
				require.NoError(t, state.SetRead(id, state.Items[0].Revision, false))
				require.False(t, state.Items[0].Read)
			} else {
				require.Equal(t, before, state)
			}
		})
	}
}

func TestCatchUp(t *testing.T) {
	for _, tt := range []struct {
		name       string
		revision   uint64
		checkpoint uint64
		wantErr    bool
		changed    bool
	}{
		{"advance", 1, 0, false, true},
		{"no-op", 0, 0, false, false},
		{"future", 2, 0, true, false},
		{"backwards", 0, 1, true, false},
		{"repeat", 1, 1, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
			state.CaughtUp = tt.checkpoint
			before := cloneState(t, state)
			err := state.CatchUp(tt.revision)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrConflict)
			} else {
				require.NoError(t, err)
			}
			if tt.changed {
				require.Equal(t, tt.revision, state.CaughtUp)
				require.Equal(t, before.Revision+1, state.Revision)
				require.Equal(t, before.Items, state.Items)
			} else {
				require.Equal(t, before, state)
			}
		})
	}
}

func TestRetention(t *testing.T) {
	for _, tt := range []struct {
		name     string
		inactive int
		age      time.Duration
		want     int
	}{
		{"under count limit", 499, time.Hour, 499},
		{"at count limit", 500, time.Hour, 500},
		{"over count limit", 503, time.Hour, 500},
		{"at age limit", 2, 30 * 24 * time.Hour, 2},
		{"over age limit", 2, 30*24*time.Hour + time.Nanosecond, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			active := observation("active", "paused")
			state.Reconcile([]Observation{active}, observedTime.Add(-90*24*time.Hour), false)
			activeID := state.Items[0].ID
			for n := range tt.inactive {
				o := observation(fmt.Sprintf("completed-%d", n), "completed")
				state.Reconcile([]Observation{active, o}, observedTime.Add(-tt.age), false)
			}
			state.Reconcile([]Observation{active}, observedTime, false)
			require.Len(t, state.Items, tt.want+1)
			require.Equal(t, activeID, state.Items[0].ID)
			require.True(t, state.Items[0].Active)
			require.Equal(t, observedTime.Add(-90*24*time.Hour), state.Items[0].ChangedAt)
			if tt.inactive > 500 {
				sources := map[string]bool{}
				for _, item := range state.Items[1:] {
					sources[item.Source] = true
				}
				for n := range tt.inactive - 500 {
					require.False(t, sources[fmt.Sprintf("completed-%d", n)])
				}
				require.True(t, sources[fmt.Sprintf("completed-%d", tt.inactive-1)])
			}
			require.False(t, state.Reconcile([]Observation{active}, observedTime, false))
		})
	}
}

func TestStartupQuestionRetention(t *testing.T) {
	for _, tt := range []struct {
		name   string
		age    time.Duration
		active bool
		want   int
	}{
		{"active old question is interrupted", 90 * 24 * time.Hour, true, 1},
		{"recent inactive question is retired", time.Hour, false, 1},
		{"expired inactive question stays pruned", 31 * 24 * time.Hour, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var state State
			then := observedTime.Add(-tt.age)
			state.Reconcile([]Observation{observation("old-invocation", "question")}, then, false)
			if !tt.active {
				state.Reconcile(nil, then, false)
			}
			require.True(t, state.Reconcile(nil, observedTime, true))
			require.Len(t, state.Items, tt.want)
			if tt.want > 0 {
				require.False(t, state.Items[0].Active)
				require.Equal(t, "interrupted", state.Items[0].InactiveReason)
			}
		})
	}
}

func TestCompletionIdentityRetention(t *testing.T) {
	var state State
	o := observation("run-1", "completion")
	o.OccurredAt = observedTime.Add(-time.Hour)
	require.True(t, state.Reconcile([]Observation{o}, observedTime, true))
	require.Len(t, state.SeenCompletions, 1)
	require.NoError(t, state.SetRead(state.Items[0].ID, state.Items[0].Revision, true))
	state = cloneState(t, state)
	require.False(t, state.Reconcile([]Observation{o}, observedTime.Add(time.Minute), true))
	require.True(t, state.Items[0].Read)
	now := observedTime.Add(31 * 24 * time.Hour)
	require.True(t, state.Reconcile([]Observation{o}, now, true))
	require.Empty(t, state.Items)
	require.Empty(t, state.SeenCompletions)
	state = cloneState(t, state)
	require.False(t, state.Reconcile([]Observation{o}, now.Add(time.Minute), true))
	require.Empty(t, state.Items)
	require.Empty(t, state.SeenCompletions)
}

func TestReadDoesNotExtendRetention(t *testing.T) {
	var state State
	then := observedTime.Add(-31 * 24 * time.Hour)
	state.Reconcile([]Observation{observation("status", "paused")}, then, false)
	state.Reconcile(nil, then, false)
	item := state.Items[0]
	require.NoError(t, state.SetRead(item.ID, item.Revision, true))
	require.True(t, state.Reconcile(nil, observedTime, false))
	require.Empty(t, state.Items)
}

func TestActiveItemsHaveNoCountLimit(t *testing.T) {
	observations := make([]Observation, 600)
	for n := range observations {
		observations[n] = observation(fmt.Sprintf("active-%d", n), "paused")
	}
	var state State
	state.Reconcile(observations, observedTime, false)
	require.False(t, state.Reconcile(observations, observedTime.Add(90*24*time.Hour), false))
	require.Len(t, state.Items, len(observations))
}

func TestSnapshotJSON(t *testing.T) {
	var state State
	state.Reconcile([]Observation{observation("status", "paused")}, observedTime, false)
	data, err := json.Marshal(state)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Len(t, fields, 5)
	for _, field := range []string{"version", "revision", "caught_up", "observed_at", "items"} {
		require.Contains(t, fields, field)
	}
	var items []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["items"], &items))
	require.Len(t, items[0], 15)
	for _, field := range []string{"id", "source", "ticket_id", "condition", "revision", "change_revision", "source_revision", "title", "detail", "identity_limited", "first_seen", "changed_at", "active", "read", "inactive_reason"} {
		require.Contains(t, items[0], field)
	}
}

func cloneState(t *testing.T, state State) State {
	t.Helper()
	data, err := json.Marshal(state)
	require.NoError(t, err)
	var cloned State
	require.NoError(t, json.Unmarshal(data, &cloned))
	return cloned
}
