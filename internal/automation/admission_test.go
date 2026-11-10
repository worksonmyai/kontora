package automation

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAllocationDispositionSurvivesRestart(t *testing.T) {
	for _, tc := range []struct {
		name               string
		decision           func(uint64) Admission
		oldPhase, newPhase string
	}{
		{"skip", func(uint64) Admission { return Admission{SkipReason: "overlap"} }, "queued", "terminal"},
		{"supersede", func(n uint64) Admission { return Admission{Supersede: []uint64{n}} }, "terminal", "queued"},
		{"stop queued", func(n uint64) Admission { return Admission{StopQueued: []uint64{n}} }, "stopping", "queued"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id, now := openTestStore(t)
			first, _, err := s.Allocate(id, "manual", "first", time.Time{}, time.Time{}, now)
			require.NoError(t, err)
			second, _, err := s.Allocate(id, "manual", "second", time.Time{}, time.Time{}, now.Add(time.Second), tc.decision(first.N))
			require.NoError(t, err)
			require.Equal(t, tc.newPhase, second.Phase)
			root, scope := filepath.Dir(s.dir), s.state.Scope
			require.NoError(t, s.Close())
			reopened, err := Open(root, scope)
			require.NoError(t, err)
			defer func() { require.NoError(t, reopened.Close()) }()
			old, err := reopened.GetRun(id, first.N)
			require.NoError(t, err)
			require.Equal(t, tc.oldPhase, old.Phase)
			replay, found, err := reopened.Allocate(id, "manual", "second", time.Time{}, time.Time{}, now.Add(time.Minute))
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, tc.newPhase, replay.Phase)
			if tc.newPhase == "terminal" {
				require.Equal(t, "skipped", replay.Outcome)
			}
			if tc.oldPhase == "stopping" {
				require.Equal(t, "replaced by a newer run", old.StopReason)
			}
		})
	}
}

func TestAutomationIDNamespace(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"aut-1234", false},
		{"aut-0123456789abcdef0123456789abcdef", true},
		{"aut-0123456789ABCDEF0123456789ABCDEF", false},
		{"kon-0123456789abcdef0123456789abcdef", false},
	} {
		t.Run(tc.id, func(t *testing.T) { require.Equal(t, tc.want, IsID(tc.id)) })
	}
}
