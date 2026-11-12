package ticket

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecutionActivity(t *testing.T) {
	for _, tc := range []struct {
		status, snapshot Status
		active           bool
	}{
		{StatusInProgress, "", true},
		{StatusInProgress, "stale", true},
		{"awaiting_data", "awaiting_data", true},
		{"awaiting_data", "", false},
		{"awaiting_data", "review", false},
		{StatusTodo, "awaiting_data", false},
		{StatusDone, StatusDone, false},
		{StatusHumanReview, StatusHumanReview, false},
		{StatusLegacyClosed, StatusLegacyClosed, false},
		{"next", "next", false},
		{"retry", "retry", false},
		{"back", "back", false},
		{"Not_valid", "Not_valid", false},
		{"", "", false},
	} {
		t.Run(fmt.Sprintf("%s/%s", tc.status, tc.snapshot), func(t *testing.T) {
			tk, err := ParseBytes(fmt.Appendf(nil, "---\nid: tst-001\nstatus: %q\nrunning_status: %q\n---\n# Ticket\n", tc.status, tc.snapshot))
			require.NoError(t, err)
			assert.Equal(t, tc.active, tk.IsRunning())
			out, err := tk.Marshal()
			require.NoError(t, err)
			parsed, err := ParseBytes(out)
			require.NoError(t, err)
			assert.Equal(t, tc.active, parsed.IsRunning())
		})
	}
}

func TestRunningStatusMutation(t *testing.T) {
	for _, source := range []struct {
		name, fields string
	}{
		{"direct", "running_status: awaiting_data\n"},
		{"inherited", "defaults: &defaults\n  running_status: awaiting_data\n<<: *defaults\n"},
		{"direct masks inherited", "defaults: &defaults\n  running_status: review\n<<: *defaults\nrunning_status: awaiting_data\n"},
		{"anchored marker", "running_status: &run \"awaiting_data\"\nmetadata: &metadata\n  snapshot: *run # original value\n  nested: [*run, {snapshot: *run}]\n  by_status: {*run: original}\nmetadata_copy: *metadata\nmerged_metadata:\n  <<: *metadata\n"},
		{"anchored inherited marker", "defaults: &defaults\n  running_status: &run awaiting_data\n<<: *defaults\nmetadata: {snapshot: *run}\n"},
		{"anchored marker masks inherited", "defaults: &defaults\n  running_status: review\n<<: *defaults\nrunning_status: &run awaiting_data\nmetadata: {snapshot: *run}\n"},
		{"aliased marker", "custom_status: &run awaiting_data\nrunning_status: *run\nmetadata: {snapshot: *run}\n"},
		{"anchored marker key", "&key running_status: awaiting_data\nmetadata: {key: *key}\n"},
		{"reused anchor name", "running_status: &run awaiting_data\nmetadata: {snapshot: *run}\nother_status: &run parked\nother_copy: *run\n"},
	} {
		for _, mutation := range []struct {
			name             string
			apply            func(*Ticket) error
			status, snapshot Status
			wantErr          bool
		}{
			{"park same name", func(tk *Ticket) error { return tk.SetField("status", "awaiting_data") }, "awaiting_data", "", false},
			{"pause", func(tk *Ticket) error { return tk.SetField("status", "paused") }, StatusPaused, "", false},
			{"queue", func(tk *Ticket) error { return tk.SetField("status", "todo") }, StatusTodo, "", false},
			{"ordinary default", func(tk *Ticket) error { return tk.SetField("status", "in_progress") }, StatusInProgress, "", false},
			{"claim custom", func(tk *Ticket) error { return tk.SetRunningStatus("review") }, "review", "review", false},
			{"claim default", func(tk *Ticket) error { return tk.SetRunningStatus(StatusInProgress) }, StatusInProgress, "", false},
			{"delete marker", func(tk *Ticket) error { return tk.DeleteField("running_status") }, "awaiting_data", "", false},
			{"delete status", func(tk *Ticket) error { return tk.DeleteField("status") }, "", "", false},
			{"summary", func(tk *Ticket) error { return tk.SetField("summary", "Waiting for collection") }, "awaiting_data", "awaiting_data", false},
			{"note", func(tk *Ticket) error { tk.SetBody(tk.Body + "\nCollection still running.\n"); return nil }, "awaiting_data", "awaiting_data", false},
			{"reject typed status", func(tk *Ticket) error { return tk.SetField("status", []string{"todo"}) }, "awaiting_data", "awaiting_data", true},
			{"reject reserved claim", func(tk *Ticket) error { return tk.SetRunningStatus("next") }, "awaiting_data", "awaiting_data", true},
			{"reject terminal claim", func(tk *Ticket) error { return tk.SetRunningStatus(StatusDone) }, "awaiting_data", "awaiting_data", true},
			{"reject epic field", func(tk *Ticket) error { return tk.SetField("epic_execution", "unknown") }, "awaiting_data", "awaiting_data", true},
		} {
			t.Run(source.name+"/"+mutation.name, func(t *testing.T) {
				tk, err := ParseBytes([]byte("---\nid: tst-001\nstatus: awaiting_data\n" + source.fields + "custom: preserved # keep me\nunrelated: &unrelated {value: kept}\nunrelated_copy: *unrelated\n---\n# Ticket\n"))
				require.NoError(t, err)
				before, err := tk.Marshal()
				require.NoError(t, err)
				var expectedFields map[string]any
				require.NoError(t, tk.rawNode.Decode(&expectedFields))
				original := tk.Clone()
				err = mutation.apply(tk)
				if mutation.wantErr {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				assert.Equal(t, mutation.status, tk.Status)
				assert.Equal(t, mutation.snapshot, tk.RunningStatus)
				out, err := tk.Marshal()
				require.NoError(t, err)
				if mutation.wantErr {
					assert.Equal(t, before, out)
				}
				assert.Contains(t, string(out), "custom: preserved # keep me")
				assert.Contains(t, string(out), "unrelated_copy: *unrelated")
				if source.name == "anchored marker" {
					assert.Contains(t, string(out), "# original value")
					assert.Contains(t, string(out), "metadata_copy: *metadata")
				}
				parsed, err := ParseBytes(out)
				require.NoError(t, err)
				assert.Equal(t, tk.Status, parsed.Status)
				assert.Equal(t, tk.RunningStatus, parsed.RunningStatus)
				assert.Equal(t, tk.IsRunning(), parsed.IsRunning())
				var actualFields map[string]any
				require.NoError(t, parsed.rawNode.Decode(&actualFields))
				for _, key := range []string{"status", "running_status", "summary"} {
					delete(expectedFields, key)
					delete(actualFields, key)
				}
				assert.Equal(t, expectedFields, actualFields)
				unchanged, err := original.Marshal()
				require.NoError(t, err)
				assert.Equal(t, before, unchanged)
			})
		}
	}
}
