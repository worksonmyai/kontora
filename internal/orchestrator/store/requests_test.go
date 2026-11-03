package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/orchestrator"
)

func TestExecuteRequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		operator string
		payload  string
		revision uint64
		want     error
	}{
		{name: "same request", operator: "operator", payload: "assignment"},
		{name: "different content", operator: "operator", payload: "other", want: orchestrator.ErrConflict},
		{name: "different operator", operator: "worker", payload: "assignment", want: orchestrator.ErrConflict},
		{name: "different revision", operator: "operator", payload: "assignment", revision: 1, want: orchestrator.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, "board")
			require.NoError(t, err)
			calls := 0
			change := func(state orchestrator.State) (orchestrator.State, []byte, error) {
				calls++
				state.Revision++
				return state, []byte("recorded outcome"), nil
			}
			first, err := s.ExecuteRequest("request", "operator", []byte("assignment"), 0, change)
			require.NoError(t, err)
			require.NoError(t, s.Close())
			s, err = Open(dir, "board")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			again, err := s.ExecuteRequest("request", tc.operator, []byte(tc.payload), tc.revision, change)
			require.ErrorIs(t, err, tc.want)
			if tc.want == nil {
				require.Equal(t, first, again)
			}
			require.Equal(t, 1, calls)
			state, err := s.State()
			require.NoError(t, err)
			require.EqualValues(t, 1, state.Revision)
		})
	}
}

func TestExecuteRequestConcurrencyAndFailure(t *testing.T) {
	for _, mode := range []string{"concurrent", "stale", "callback failure", "uncertain journal", "missing response"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, "board")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			calls := 0
			change := func(state orchestrator.State) (orchestrator.State, []byte, error) {
				calls++
				state.Revision++
				return state, []byte("outcome"), nil
			}
			switch mode {
			case "concurrent":
				var wg sync.WaitGroup
				for range 12 {
					wg.Go(func() {
						body, err := s.ExecuteRequest("r", "operator", []byte("body"), 0, change)
						require.NoError(t, err)
						require.Equal(t, "outcome", string(body))
					})
				}
				wg.Wait()
				require.Equal(t, 1, calls)
			case "stale":
				_, err = s.ExecuteRequest("r", "operator", nil, 1, change)
				require.ErrorIs(t, err, orchestrator.ErrStale)
				require.Zero(t, calls)
			case "callback failure":
				_, err = s.ExecuteRequest("r", "operator", nil, 0, func(state orchestrator.State) (orchestrator.State, []byte, error) {
					return state, nil, fmt.Errorf("rejected")
				})
				require.EqualError(t, err, "rejected")
				state, err := s.State()
				require.NoError(t, err)
				require.Empty(t, state.Requests)
			case "uncertain journal":
				s.fault = func(point string) error {
					if point == "journal.after_sync" {
						return errors.New("interrupted")
					}
					return nil
				}
				_, err = s.ExecuteRequest("r", "operator", nil, 0, change)
				require.ErrorIs(t, err, ErrRecovery)
				require.NoError(t, s.Close())
				s, err = Open(dir, "board")
				require.NoError(t, err)
				_, err = s.ExecuteRequest("r", "operator", nil, 0, change)
				require.NoError(t, err)
				require.Equal(t, 1, calls)
			case "missing response":
				_, err = s.ExecuteRequest("r", "operator", nil, 0, change)
				require.NoError(t, err)
				state, err := s.State()
				require.NoError(t, err)
				ref := state.Requests["r"].Response
				path, err := s.artifactPath(ref)
				require.NoError(t, err)
				require.NoError(t, s.Close())
				require.NoError(t, os.Remove(path))
				_, err = Open(filepath.Clean(dir), "board")
				require.ErrorIs(t, err, ErrRecovery)
			}
		})
	}
}
