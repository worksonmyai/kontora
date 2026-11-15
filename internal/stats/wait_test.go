package stats

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWaitingAccountingAndContinuationQuality(t *testing.T) {
	run := func(outcome, visit string, continuation bool, days int) Run {
		r := spent(mkRun("work", "agent", 0, ago(days, 9), time.Minute), 100, 20)
		r.WorkflowExecutionID, r.NodeVisitID, r.RunOutcome, r.Continuation = "exec", visit, outcome, continuation
		return r
	}
	for _, tc := range []struct {
		name                      string
		history                   []Run
		waiting, complete, failed int
		firstPass, retries        float64
	}{
		{"wait only has no quality judgment", []Run{run("waiting", "v1", false, 1)}, 1, 0, 0, 0, 0},
		{"multiple suspensions keep first pass", []Run{run("waiting", "v1", false, 2), run("waiting", "v1", true, 1), run("complete", "v1", true, 1)}, 2, 1, 0, 100, 0},
		{"failed continuation retry is real", []Run{run("waiting", "v1", false, 2), run("failed", "v1", true, 1), run("complete", "v1", false, 1)}, 1, 1, 1, 0, 1},
		{"failed attempt before wait remains failed", []Run{run("failed", "v1", false, 2), run("waiting", "v1", false, 1), run("complete", "v1", true, 1)}, 1, 1, 1, 0, 1},
		{"waiting visit does not dilute another visit", []Run{run("waiting", "v1", false, 1), run("complete", "v2", false, 1)}, 1, 1, 0, 100, 0},
		{"prior failure outside window is retained", []Run{run("failed", "v1", false, 9), run("waiting", "v1", false, 8), run("complete", "v1", true, 1)}, 0, 1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := Compute([]Ticket{{ID: "ticket", History: tc.history}}, Options{Now: testNow, Days: 7})
			stage, agent := result.Stages[0], result.Agents[0]
			runs := tc.waiting + tc.complete + tc.failed
			require.Equal(t, runs, result.Totals.Runs)
			require.Equal(t, runs, stage.Runs)
			require.Equal(t, runs, agent.Runs)
			require.Equal(t, tc.waiting, stage.Waiting)
			require.Equal(t, tc.complete, stage.Complete)
			require.Equal(t, tc.failed, stage.Failed)
			require.Zero(t, stage.Unknown)
			require.Equal(t, int64(runs*120), stage.Tokens)
			require.Equal(t, int64(runs*100), result.Totals.TokensIn)
			require.Equal(t, time.Minute.Milliseconds(), stage.P50MS)
			require.Equal(t, 100.0, stage.Share)
			if tc.complete+tc.failed == 0 {
				require.Zero(t, result.Totals.FirstPassVisits)
				require.Zero(t, agent.FirstPassVisits)
			} else {
				require.Positive(t, result.Totals.FirstPassVisits)
				require.Positive(t, agent.FirstPassVisits)
			}
			require.Equal(t, tc.firstPass, result.Totals.FirstPassPct)
			require.Equal(t, tc.firstPass, agent.FirstPassPct)
			require.InDelta(t, tc.retries*100/float64(runs), stage.RetryPct, 0.001)
			require.Equal(t, tc.retries, agent.RetriesPerTicket)
		})
	}
}
