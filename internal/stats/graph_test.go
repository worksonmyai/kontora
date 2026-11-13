package stats

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGraphVisitQuality(t *testing.T) {
	graphRun := func(execution, visit string, index, days int, outcome string) Run {
		r := spent(mkRun("review", "claude", index, ago(days, 9), time.Minute), 100, 10)
		r.WorkflowExecutionID, r.NodeVisitID, r.RunOutcome = execution, visit, outcome
		return r
	}
	for _, tc := range []struct {
		name      string
		history   []Run
		runs      int
		firstPass float64
		retryPct  float64
		retries   float64
	}{
		{
			name: "intentional loop visits are independent first attempts",
			history: []Run{
				graphRun("exec", "visit-1", 0, 2, "complete"),
				graphRun("exec", "visit-2", 1, 1, "complete"),
			},
			runs: 2, firstPass: 100,
		},
		{
			name: "same visit retry ignores a zero run index",
			history: []Run{
				graphRun("exec", "visit-1", 0, 2, "failed"),
				graphRun("exec", "visit-1", 0, 1, "complete"),
				graphRun("exec", "visit-2", 2, 1, "complete"),
			},
			runs: 3, firstPass: 50, retryPct: 100.0 / 3, retries: 1,
		},
		{
			name: "restart creates a new execution despite retained run counters",
			history: []Run{
				graphRun("old", "visit-1", 0, 2, "failed"),
				graphRun("new", "visit-1", 1, 1, "complete"),
			},
			runs: 2, firstPass: 50,
		},
		{
			name: "retry of a visit outside the window remains a retry",
			history: []Run{
				graphRun("exec", "visit-1", 0, 8, "failed"),
				graphRun("exec", "visit-1", 0, 1, "complete"),
			},
			runs: 1, retryPct: 100, retries: 1,
		},
		{
			name: "unknown graph outcome is not verified",
			history: []Run{
				graphRun("exec", "visit-1", 0, 2, ""),
				graphRun("exec", "visit-2", 1, 1, "complete"),
			},
			runs: 2, firstPass: 50,
		},
		{
			name: "legacy counters retain their meaning beside graph history",
			history: []Run{
				mkRun("review", "claude", 1, ago(2, 9), time.Minute),
				graphRun("exec", "visit-1", 2, 1, "complete"),
			},
			runs: 2, firstPass: 50, retryPct: 50, retries: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Compute([]Ticket{{ID: "ticket", History: tc.history}}, Options{Now: testNow, Days: 7})
			require.Equal(t, tc.runs, r.Totals.Runs)
			require.InDelta(t, tc.firstPass, r.Totals.FirstPassPct, 0.001)
			require.InDelta(t, tc.firstPass, r.Agents[0].FirstPassPct, 0.001)
			require.InDelta(t, tc.retryPct, r.Stages[0].RetryPct, 0.001)
			require.InDelta(t, tc.retries, r.Agents[0].RetriesPerTicket, 0.001)
		})
	}
}

func TestGraphAnnotationsDoNotConsumeVisits(t *testing.T) {
	run := spent(mkRun("review", "claude", 4, ago(1, 9), time.Minute), 100, 10)
	run.WorkflowExecutionID, run.NodeVisitID = "exec", "visit"
	annotation := run
	annotation.Kind = "annotation"
	r := Compute([]Ticket{{ID: "ticket", History: []Run{annotation, run}}}, Options{Now: testNow, Days: 7})
	require.Equal(t, 1, r.Totals.Runs)
	require.Equal(t, int64(200), r.Totals.TokensIn)
	require.Equal(t, int64(110), r.Stages[0].Tokens)
	require.Equal(t, 100.0, r.Totals.FirstPassPct)
	require.Zero(t, r.Stages[0].RetryPct)
}

func TestGraphRetryAcrossAgents(t *testing.T) {
	first := mkRun("review", "first-agent", 0, ago(2, 9), time.Minute)
	first.WorkflowExecutionID, first.NodeVisitID = "exec", "visit"
	first.RunOutcome = "blocked"
	retry := mkRun("review", "replacement-agent", 0, ago(1, 9), time.Minute)
	retry.WorkflowExecutionID, retry.NodeVisitID = "exec", "visit"
	r := Compute([]Ticket{{ID: "ticket", History: []Run{first, retry}}}, Options{Now: testNow, Days: 7})
	require.Zero(t, r.Totals.FirstPassPct)
	require.Equal(t, 50.0, r.Stages[0].RetryPct)
	for _, agent := range r.Agents {
		require.Zero(t, agent.FirstPassPct)
		if agent.Name == "replacement-agent" {
			require.Equal(t, 1.0, agent.RetriesPerTicket)
		} else {
			require.Zero(t, agent.RetriesPerTicket)
		}
	}
}

func TestGraphRunnerFailureIsNotFirstPass(t *testing.T) {
	run := mkRun("review", "claude", 9, ago(1, 9), time.Minute)
	run.WorkflowExecutionID, run.NodeVisitID, run.ExitCode = "exec", "visit", 7
	r := Compute([]Ticket{{ID: "ticket", History: []Run{run}}}, Options{Now: testNow, Days: 7})
	require.Zero(t, r.Totals.FirstPassPct)
	require.Equal(t, 1, r.Stages[0].RunnerFailed)
	require.Zero(t, r.Stages[0].RetryPct, "a failed first visit is not itself a retry")
}

func TestGraphVisitIdentityIncludesTicketAndNode(t *testing.T) {
	run := mkRun("review-a", "claude", 5, ago(1, 9), time.Minute)
	run.WorkflowExecutionID, run.NodeVisitID = "exec", "visit"
	other := run
	other.Stage, other.RunOutcome = "review-b", "failed"
	r := Compute([]Ticket{
		{ID: "first", History: []Run{run, other}},
		{ID: "second", History: []Run{run}},
	}, Options{Now: testNow, Days: 7})
	require.InDelta(t, 200.0/3, r.Totals.FirstPassPct, 0.001)
	require.Zero(t, r.Agents[0].RetriesPerTicket)
	for _, stage := range r.Stages {
		require.Zero(t, stage.RetryPct)
	}
}
