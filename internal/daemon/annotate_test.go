package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

// annotateJSON is what `plannotator annotate --gate --json` prints.
func annotateJSON(decision, feedback string) string {
	m := map[string]string{"decision": decision}
	if feedback != "" {
		m["feedback"] = feedback
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// annotationTicketMD is a ticket in the given status with a two-stage pipeline
// parked at step2. extra is appended to the frontmatter.
func (h *plannotatorHarness) annotationTicketMD(id, status, extra string) string {
	return h.ticketMDWithPipeline(id, status, "pipeline: two-stage\nstage: step2\n", extra)
}

// simpleTicketMD is a ticket that runs without a pipeline, whose runs key on
// simpleStageName rather than on a stage.
func (h *plannotatorHarness) simpleTicketMD(id, status, extra string) string {
	return h.ticketMDWithPipeline(id, status, "", extra)
}

func (h *plannotatorHarness) ticketMDWithPipeline(id, status, pipeline, extra string) string {
	return `---
id: ` + id + `
kontora: true
status: ` + status + `
` + pipeline + `path: ` + h.repoDir + `
created: 2026-01-01T00:00:00Z
` + extra + `---
# Test ticket ` + id + `
`
}

// newAnnotationDaemon builds the daemon every annotation-run test needs: agent runs go
// to runner, tmux is stubbed out, and plannotator is faked by the harness.
func (h *plannotatorHarness) newAnnotationDaemon(runner RunnerFunc, opts ...Option) *Daemon {
	h.t.Helper()
	d := New(h.cfg, append([]Option{
		WithLogger(testLogger(h.t)),
		WithDebounce(50 * time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
		WithPlannotatorSpawner(h.spawner()),
		WithPlannotatorLookup(h.lookup()),
	}, opts...)...)
	d.windows = windowOps{
		list: func(string) ([]string, error) { return nil, nil },
		kill: func(string, string) error { return nil },
	}
	return d
}

// startAnnotationDaemon starts d, seeds a ticket, and waits until the daemon has
// indexed it. The returned function stops the daemon and asserts a clean exit.
func startAnnotationDaemon(t *testing.T, h *plannotatorHarness, d *Daemon, id, md string) (filePath string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	errCh := make(chan error, 1)
	go func() { errCh <- d.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)

	filePath = h.writeTicket(id+".md", md)
	require.Eventually(t, func() bool {
		_, err := d.GetTicket(id)
		return err == nil
	}, 3*time.Second, 20*time.Millisecond, "daemon should index the ticket")

	return filePath, func() {
		cancel()
		require.NoError(t, <-errCh)
	}
}

// waitForAnnotationRuns waits until the ticket records want annotation history
// entries and the annotation run has released it. Waiting on the status alone would
// pass before the run started: a annotation run restores the status it began with.
func (h *plannotatorHarness) waitForAnnotationRuns(id string, want int, status ticket.Status) *ticket.Ticket {
	h.t.Helper()
	path := filepath.Join(h.tasksDir, id+".md")
	var last *ticket.Ticket
	require.Eventually(h.t, func() bool {
		tk, err := ticket.ParseFile(path)
		if err != nil {
			return false
		}
		last = tk
		if tk.Status != status {
			return false
		}
		n := 0
		for _, e := range tk.History {
			if e.Kind == ticket.KindAnnotation {
				n++
			}
		}
		return n == want
	}, 15*time.Second, 50*time.Millisecond,
		"want %d annotation runs and status %s, last seen %+v", want, status, last)
	return last
}

// waitForPlannotatorDone waits until the in-flight entry for id is gone, which
// means the annotate goroutine finished and its state changes are on disk.
func waitForPlannotatorDone(t *testing.T, d *Daemon, id string) {
	t.Helper()
	require.Eventually(t, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		_, running := d.plannotator[id]
		return !running
	}, 3*time.Second, 20*time.Millisecond, "plannotator should finish")
}

// awaitOutcome drains events until the plannotator_finished for id arrives.
func awaitOutcome(t *testing.T, events <-chan web.TicketEvent, id string) web.TicketEvent {
	t.Helper()
	return awaitEvent(t, events, "plannotator_finished", id)
}

func awaitEvent(t *testing.T, events <-chan web.TicketEvent, typ, id string) web.TicketEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == typ && ev.Ticket.ID == id {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func TestParseAnnotateDecision(t *testing.T) {
	cases := []struct {
		name         string
		stdout       string
		wantDecision string
		wantFeedback string
		wantErr      string
	}{
		{name: "approved", stdout: annotateJSON(annotateApproved, ""), wantDecision: annotateApproved},
		{
			name:         "approved with notes",
			stdout:       annotateJSON(annotateApproved, "looks fine, one thought"),
			wantDecision: annotateApproved,
			wantFeedback: "looks fine, one thought",
		},
		{name: "dismissed", stdout: annotateJSON(annotateDismissed, ""), wantDecision: annotateDismissed},
		{
			name:         "annotated",
			stdout:       "  " + annotateJSON(annotateAnnotated, "fix the goal\nand the spec") + "\n",
			wantDecision: annotateAnnotated,
			wantFeedback: "fix the goal\nand the spec",
		},
		{name: "annotated with no feedback", stdout: annotateJSON(annotateAnnotated, ""), wantDecision: annotateAnnotated},
		{name: "empty", stdout: "   \n", wantErr: "wrote no decision"},
		{name: "plain text", stdout: "The user approved.", wantErr: "unparseable decision"},
		{name: "hook shape", stdout: `{"decision":"block","reason":"nope"}`, wantErr: `unknown decision "block"`},
		{name: "no decision key", stdout: `{"feedback":"orphan"}`, wantErr: `unknown decision ""`},
		{
			name:    "long output is cut",
			stdout:  strings.Repeat("stack trace ", 100),
			wantErr: "…",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAnnotateDecision(tc.stdout)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Less(t, len(err.Error()), 300, "a long output must not be quoted whole")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDecision, got.Decision)
			assert.Equal(t, tc.wantFeedback, got.Feedback)
		})
	}
}

// TestAnnotate_DecisionsThatChangeNothing covers every stdout the daemon must
// not act on. The ticket keeps its status, stage, and attempt, and no
// annotations file is left behind.
func TestAnnotate_DecisionsThatChangeNothing(t *testing.T) {
	cases := []struct {
		name        string
		stdout      string
		wantOutcome string
	}{
		{name: "approved", stdout: annotateJSON(annotateApproved, ""), wantOutcome: web.PlannotatorOutcomeApproved},
		{
			name:        "approved with notes",
			stdout:      annotateJSON(annotateApproved, "one thought"),
			wantOutcome: web.PlannotatorOutcomeApproved,
		},
		{name: "dismissed", stdout: annotateJSON(annotateDismissed, ""), wantOutcome: web.PlannotatorOutcomeCancelled},
		{
			name:        "annotated with empty feedback",
			stdout:      annotateJSON(annotateAnnotated, "   "),
			wantOutcome: web.PlannotatorOutcomeCancelled,
		},
		{name: "empty stdout", stdout: "", wantOutcome: web.PlannotatorOutcomeError},
		{name: "not json", stdout: "Annotation session closed.", wantOutcome: web.PlannotatorOutcomeError},
		{name: "unknown decision", stdout: `{"decision":"maybe"}`, wantOutcome: web.PlannotatorOutcomeError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			d := h.newDaemonWithSpawner()

			_, stop := startAnnotationDaemon(t, h, d, "tst-an01",
				h.annotationTicketMD("tst-an01", "open", "attempt: 2\n"))
			defer stop()

			events, unsub := d.Subscribe()
			defer unsub()

			require.NoError(t, d.StartPlannotatorAnnotate("tst-an01"))
			// The UI disables both actions off this event for as long as a session
			// is open, so it has to be broadcast before the outcome.
			awaitEvent(t, events, "plannotator_started", "tst-an01")
			h.stdoutCh <- tc.stdout
			waitForPlannotatorDone(t, d, "tst-an01")

			got := h.readTask("tst-an01.md")
			assert.Equal(t, ticket.StatusOpen, got.Status)
			assert.Equal(t, "step2", got.Stage)
			assert.Equal(t, 2, got.Attempt)
			assert.Empty(t, got.AnnotationReturnStatus)
			assert.Contains(t, got.Body, "# Test ticket tst-an01")
			assert.NoFileExists(t, annotationsPath(h.reviewsDir, "tst-an01"))

			ev := awaitOutcome(t, events, "tst-an01")
			assert.Equal(t, tc.wantOutcome, ev.Outcome)
			if tc.wantOutcome == web.PlannotatorOutcomeError && tc.stdout != "" {
				assert.Contains(t, ev.Message, strings.TrimSpace(tc.stdout),
					"the raw output belongs in the error message")
			}
		})
	}
}

// TestAnnotate_SubmittedFeedbackParksTicket pins the park: the feedback goes to
// its own file, the previous status is recorded, and the stage does not move. The
// agent blocks so the annotation run cannot race the assertions.
func TestAnnotate_SubmittedFeedbackParksTicket(t *testing.T) {
	h := newPlannotatorHarness(t)

	// The annotation run blocks, so the assertions read the parked state rather than
	// the state the run restores.
	refining := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	runner := func(ctx context.Context, _ RunnerParams) (process.Result, error) {
		once.Do(func() { close(refining) })
		select {
		case <-release:
		case <-ctx.Done():
		}
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}
	d := New(h.cfg,
		WithLogger(testLogger(t)),
		WithDebounce(50*time.Millisecond),
		WithLockPath(h.lockPath),
		WithRunner(runner),
		WithAgentLookup(passthroughAgentLookup),
		WithSkipOrphanCleanup(),
		WithPlannotatorSpawner(h.spawner()),
		WithPlannotatorLookup(h.lookup()),
	)

	_, stop := startAnnotationDaemon(t, h, d, "tst-an02",
		h.annotationTicketMD("tst-an02", "open", "attempt: 2\n"))
	defer func() {
		close(release)
		stop()
	}()

	events, unsub := d.Subscribe()
	defer unsub()

	require.NoError(t, d.StartPlannotatorAnnotate("tst-an02"))

	var params PlannotatorParams
	select {
	case params = <-h.spawnParams:
	case <-time.After(3 * time.Second):
		t.Fatal("spawner should be invoked")
	}
	assert.Equal(t, []string{"annotate", filepath.Join(h.tasksDir, "tst-an02.md"), "--gate", "--json"}, params.Args)
	assert.Equal(t, h.tasksDir, params.Dir, "the annotate target is in tickets_dir, not a review worktree")
	assert.NoDirExists(t, filepath.Join(h.wtDir, h.repoName, "tst-an02")+".plannotator",
		"annotating a ticket must not build a review worktree")

	feedback := "sharpen the goal\nand drop phase 4"
	h.stdoutCh <- annotateJSON(annotateAnnotated, feedback)

	select {
	case <-refining:
	case <-time.After(5 * time.Second):
		t.Fatal("the annotation run should start")
	}

	got := h.readTask("tst-an02.md")
	assert.Equal(t, ticket.StatusOpen, got.AnnotationReturnStatus)
	assert.Equal(t, "step2", got.Stage, "the stage must not move")
	assert.Equal(t, 0, got.Attempt)
	assert.Contains(t, []ticket.Status{ticket.StatusTodo, ticket.StatusInProgress}, got.Status)

	data, err := os.ReadFile(annotationsPath(h.reviewsDir, "tst-an02"))
	require.NoError(t, err)
	assert.Equal(t, feedback, string(data))
	assert.NoFileExists(t, filepath.Join(h.reviewsDir, "tst-an02.md"),
		"annotations must not land in the code review file")

	ev := awaitOutcome(t, events, "tst-an02")
	assert.Equal(t, web.PlannotatorOutcomeAnnotated, ev.Outcome)
}

// TestAnnotate_StatusRules covers which statuses may be annotated. Only open
// qualifies: past that point a stage has run against the ticket text, so a
// rewrite would contradict work that already happened.
func TestAnnotate_StatusRules(t *testing.T) {
	cases := []struct {
		name   string
		status string
		// unmanaged drops `kontora: true` from the ticket.
		unmanaged bool
		// extra is appended to the frontmatter.
		extra   string
		wantErr error
	}{
		{name: "open", status: "open"},
		{name: "todo", status: "todo", wantErr: web.ErrInvalidState},
		{name: "paused", status: "paused", wantErr: web.ErrInvalidState},
		{name: "human_review", status: "human_review", wantErr: web.ErrInvalidState},
		{name: "custom status", status: "blocked", wantErr: web.ErrInvalidState},
		{name: "in_progress", status: "in_progress", wantErr: web.ErrInvalidState},
		{name: "done", status: "done", wantErr: web.ErrInvalidState},
		{name: "cancelled", status: "cancelled", wantErr: web.ErrInvalidState},
		{name: "archived", status: "archived", wantErr: web.ErrInvalidState},
		{
			// Allowed: the park adopts the ticket, so the scheduler picks it up.
			name:      "uninitialized ticket",
			status:    "open",
			unmanaged: true,
		},
		{
			// A second pass would overwrite the pending annotations and record the
			// parked status as the one to return to.
			name:    "already parked for an annotation run",
			status:  "open",
			extra:   "annotation_return_status: open\n",
			wantErr: web.ErrInvalidState,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			h.cfg.Statuses = []string{"blocked"}
			// A todo ticket is otherwise picked up and running by the time the
			// annotate call reads its status.
			h.cfg.AutoPickUp = new(false)
			d := h.newDaemonWithSpawner()

			md := h.annotationTicketMD("tst-an03", tc.status, tc.extra)
			if tc.unmanaged {
				md = strings.Replace(md, "kontora: true\n", "", 1)
			}
			_, stop := startAnnotationDaemon(t, h, d, "tst-an03", md)
			defer stop()

			// The dashboard renders its button from can_annotate, so the projection
			// has to answer the same question the endpoint does.
			info, getErr := d.GetTicket("tst-an03")
			require.NoError(t, getErr)
			assert.Equal(t, tc.wantErr == nil, info.CanAnnotate)

			err := d.StartPlannotatorAnnotate("tst-an03")
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				assert.Equal(t, int32(0), h.callCount.Load(), "no plannotator process may be spawned")
				return
			}
			require.NoError(t, err)
			require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
				3*time.Second, 20*time.Millisecond, "spawner should be invoked")
			h.stdoutCh <- annotateJSON(annotateDismissed, "")
			waitForPlannotatorDone(t, d, "tst-an03")
		})
	}
}

// TestAnnotate_InFlightGuard covers the shared in-flight map: a review and an
// annotation cannot run against the same ticket at once, in either order.
func TestAnnotate_InFlightGuard(t *testing.T) {
	cases := []struct {
		name   string
		first  func(d *Daemon, id string) error
		second func(d *Daemon, id string) error
		// status is what the ticket starts in, because annotate wants open and
		// review wants human_review. moveTo, when set, is the status the ticket is
		// moved to before the second call: the two passes are never offered in the
		// same status, so only a move mid-session can make them contend.
		status string
		moveTo string
		// firstOutput closes the session the first call opened without asking for
		// any work: the two passes read their stdout differently.
		firstOutput string
	}{
		{
			name:        "annotate blocks annotate",
			first:       (*Daemon).StartPlannotatorAnnotate,
			second:      (*Daemon).StartPlannotatorAnnotate,
			status:      "open",
			firstOutput: annotateJSON(annotateDismissed, ""),
		},
		{
			name:        "annotate blocks review",
			first:       (*Daemon).StartPlannotatorAnnotate,
			second:      (*Daemon).StartPlannotatorReview,
			status:      "open",
			moveTo:      "human_review",
			firstOutput: annotateJSON(annotateDismissed, ""),
		},
		{
			name:        "review blocks annotate",
			first:       (*Daemon).StartPlannotatorReview,
			second:      (*Daemon).StartPlannotatorAnnotate,
			status:      "human_review",
			moveTo:      "open",
			firstOutput: plannotatorCancelledMarker,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			d := h.newDaemonWithSpawner()

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			errCh := make(chan error, 1)
			go func() { errCh <- d.Run(ctx) }()
			time.Sleep(200 * time.Millisecond)
			defer func() {
				cancel()
				require.NoError(t, <-errCh)
			}()

			h.seedReviewWorktree("tst-an04")
			h.writeTicket("tst-an04.md", h.reviewTaskMD("tst-an04", tc.status, "kontora/tst-an04"))
			require.Eventually(t, func() bool {
				_, err := d.GetTicket("tst-an04")
				return err == nil
			}, 3*time.Second, 20*time.Millisecond)

			require.NoError(t, tc.first(d, "tst-an04"))

			if tc.moveTo != "" {
				h.writeTicket("tst-an04.md", h.reviewTaskMD("tst-an04", tc.moveTo, "kontora/tst-an04"))
				require.Eventually(t, func() bool {
					info, err := d.GetTicket("tst-an04")
					return err == nil && info.Status == tc.moveTo
				}, 3*time.Second, 20*time.Millisecond, "daemon should see the moved status")
			}

			assert.ErrorIs(t, tc.second(d, "tst-an04"), web.ErrPlannotatorInFlight)

			require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
				3*time.Second, 20*time.Millisecond, "only one process may be spawned")

			h.stdoutCh <- tc.firstOutput
			waitForPlannotatorDone(t, d, "tst-an04")
			assert.Equal(t, int32(1), h.callCount.Load())
		})
	}
}

func TestAnnotate_UnknownTicketAndMissingBinary(t *testing.T) {
	cases := []struct {
		name        string
		id          string
		lookupFails bool
		wantErr     error
	}{
		{name: "unknown ticket", id: "nonexistent", wantErr: web.ErrTicketNotFound},
		{name: "unsafe id", id: "../escape", wantErr: web.ErrTicketNotFound},
		{name: "missing binary", id: "tst-an05", lookupFails: true, wantErr: web.ErrPlannotatorBinary},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			h.lookupFails = tc.lookupFails
			d := h.newDaemonWithSpawner()

			_, stop := startAnnotationDaemon(t, h, d, "tst-an05",
				h.annotationTicketMD("tst-an05", "open", ""))
			defer stop()

			assert.ErrorIs(t, d.StartPlannotatorAnnotate(tc.id), tc.wantErr)
			assert.Equal(t, int32(0), h.callCount.Load())
		})
	}
}

// annotationRun captures what a annotation run was invoked with.
type annotationRun struct {
	mu     sync.Mutex
	spawns []RunnerParams
}

func (a *annotationRun) record(p RunnerParams) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.spawns = append(a.spawns, p)
}

func (a *annotationRun) all() []RunnerParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.spawns)
}

// TestAnnotate_RewritesTicketAndRestoresStatus drives the whole loop:
// annotations in, the agent rewrites the ticket body, the ticket returns to the
// status it was annotated in.
func TestAnnotate_RewritesTicketAndRestoresStatus(t *testing.T) {
	h := newPlannotatorHarness(t)
	h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
	filePath := filepath.Join(h.tasksDir, "tst-an06.md")

	var runs annotationRun
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		tk, err := ticket.ParseFile(filePath)
		require.NoError(t, err)
		tk.SetBody("# Test ticket tst-an06\n\nrewritten from the annotations\n")
		// The operational appendix asks every run for a summary, so the agent
		// writes one the way `kontora summary` does.
		require.NoError(t, tk.SetField("summary", "answered the notes"))
		data, mErr := tk.Marshal()
		require.NoError(t, mErr)
		require.NoError(t, os.WriteFile(filePath, data, 0o644))
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}

	var finalSummaries atomic.Int32
	d := h.newAnnotationDaemon(runner,
		WithFinalSummarySpawner(func(context.Context, FinalSummaryParams) (string, error) {
			finalSummaries.Add(1)
			return "should not run", nil
		}),
	)

	h.seedReviewWorktree("tst-an06")
	_, stop := startAnnotationDaemon(t, h, d, "tst-an06",
		h.annotationTicketMD("tst-an06", "open",
			"branch: kontora/tst-an06\nsummary: coded it\nfinal_summary: the whole ticket\n"+
				"history:\n  - stage: step2\n    agent: agent2\n    exit_code: 0\n"))
	defer stop()

	require.NoError(t, d.StartPlannotatorAnnotate("tst-an06"))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

	got := h.waitForAnnotationRuns("tst-an06", 1, ticket.StatusOpen)
	assert.Empty(t, got.AnnotationReturnStatus, "the marker is cleared on success")
	assert.Equal(t, "step2", got.Stage, "the pipeline must not advance")
	assert.Empty(t, got.LastError)
	assert.Contains(t, got.Body, "rewritten from the annotations")
	assert.Equal(t, "coded it", got.Summary, "the ticket keeps the summary of the work it did")
	assert.Equal(t, "the whole ticket", got.FinalSummary)
	assert.NoFileExists(t, annotationsPath(h.reviewsDir, "tst-an06"),
		"the annotations are removed only after a successful run")

	require.Len(t, got.History, 2)
	entry := got.History[1]
	assert.Equal(t, ticket.KindAnnotation, entry.Kind)
	assert.Equal(t, "step2", entry.Stage)
	assert.Equal(t, "agent2", entry.Agent)
	assert.Equal(t, 0, entry.ExitCode)
	assert.Equal(t, "answered the notes", entry.Summary,
		"the history row describes this run, not the stage before it")

	spawns := runs.all()
	require.Len(t, spawns, 1)
	prompt := renderedPrompt(spawns[0])
	assert.Contains(t, prompt, "sharpen the goal", "the annotations reach the agent")
	assert.Contains(t, prompt, filePath, "the prompt names the exact ticket file")
	assert.Contains(t, prompt, "Edit only "+filePath)
	assert.Contains(t, prompt, "do not commit or push anything")
	assert.NotContains(t, prompt, "do step2 for", "the stage prompt must not be used")

	// No final-summary pass: this run did not touch the work the ticket did.
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(0), finalSummaries.Load())
}

// TestAnnotate_InheritsStageModel: an annotation run borrows the model and the
// reasoning effort of the stage it runs under, so a stage set to run cheaply
// stays cheap when it answers notes. The stage prompt is still left behind:
// that one describes the work this run must not do.
func TestAnnotate_InheritsStageModel(t *testing.T) {
	const id = "tst-an14"
	h := newPlannotatorHarness(t)
	h.cfg.Agents["agent2"] = config.Agent{Binary: "claude", Args: []string{"--model", "opus"}}
	h.cfg.Stages["step2"] = config.Stage{
		Prompt: "do step2 for {{ .Ticket.ID }}",
		Model:  config.PerAgent{ByAgent: map[string]string{"claude": "haiku"}},
		Effort: config.PerAgent{Any: "low"},
	}

	var runs annotationRun
	d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	})

	h.seedReviewWorktree(id)
	_, stop := startAnnotationDaemon(t, h, d, id,
		h.annotationTicketMD(id, "open", "branch: kontora/"+id+"\n"+
			"history:\n  - stage: step2\n    agent: agent2\n    exit_code: 0\n"))
	defer stop()

	require.NoError(t, d.StartPlannotatorAnnotate(id))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

	h.waitForAnnotationRuns(id, 1, ticket.StatusOpen)

	spawns := runs.all()
	require.Len(t, spawns, 1)
	assert.Equal(t, "haiku", modelArg(t, spawns[0].Args))
	assert.Equal(t, "low", flagArg(t, spawns[0].Args, "--effort"))
	assert.NotContains(t, renderedPrompt(spawns[0]), "do step2 for")
}

// TestAnnotate_FailedRunKeepsFeedback covers the retry path: a nonzero
// exit pauses the ticket but keeps both the marker and the annotations, and the
// retry runs against the same feedback.
func TestAnnotate_FailedRunKeepsFeedback(t *testing.T) {
	h := newPlannotatorHarness(t)
	h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}

	var runs annotationRun
	var exitCode atomic.Int32
	exitCode.Store(1)
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		return process.Result{ExitCode: int(exitCode.Load()), StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}
	d := h.newAnnotationDaemon(runner)

	h.seedReviewWorktree("tst-an07")
	_, stop := startAnnotationDaemon(t, h, d, "tst-an07",
		h.annotationTicketMD("tst-an07", "open", "branch: kontora/tst-an07\n"))
	defer stop()

	require.NoError(t, d.StartPlannotatorAnnotate("tst-an07"))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

	got := h.waitForAnnotationRuns("tst-an07", 1, ticket.StatusPaused)
	assert.Equal(t, ticket.StatusOpen, got.AnnotationReturnStatus, "the marker survives a failure")
	assert.Contains(t, got.LastError, "annotation agent exited with code 1")
	assert.FileExists(t, annotationsPath(h.reviewsDir, "tst-an07"), "the feedback survives a failure")
	require.Len(t, got.History, 1)
	assert.Equal(t, ticket.KindAnnotation, got.History[0].Kind)

	// The retry runs against the same annotations and records one more entry.
	exitCode.Store(0)
	_, err := d.svc.Retry("tst-an07")
	require.NoError(t, err)

	got = h.waitForAnnotationRuns("tst-an07", 2, ticket.StatusOpen)
	assert.Empty(t, got.AnnotationReturnStatus)
	assert.NoFileExists(t, annotationsPath(h.reviewsDir, "tst-an07"))
	require.Len(t, got.History, 2, "one annotation entry per annotation run")
	assert.Equal(t, ticket.KindAnnotation, got.History[1].Kind)

	spawns := runs.all()
	require.Len(t, spawns, 2)
	for i, p := range spawns {
		assert.Contains(t, renderedPrompt(p), "sharpen the goal", "run %d sees the annotations", i)
	}
}

// TestAnnotate_WorkDir covers where a annotation run works. A ticket with a worktree
// reuses it; a ticket annotated before its first stage runs in the repository and
// is given no branch; a ticket that names no repository runs in tickets_dir.
func TestAnnotate_WorkDir(t *testing.T) {
	cases := []struct {
		name string
		// worktree creates the ticket's worktree and branch before the run.
		worktree bool
		// noPath drops the `path` field, leaving the run no repository to work in.
		noPath bool
		extra  string
		status string
	}{
		{
			name:     "existing worktree is reused",
			worktree: true,
			status:   "open",
			extra:    "branch: kontora/tst-an08\n",
		},
		{
			name:   "no prior run works in the repository",
			status: "open",
		},
		{
			name:   "no path works in tickets_dir",
			status: "open",
			noPath: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}

			var runs annotationRun
			runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			}
			d := h.newAnnotationDaemon(runner)

			if tc.worktree {
				h.seedReviewWorktree("tst-an08")
			}
			md := h.annotationTicketMD("tst-an08", tc.status, tc.extra)
			if tc.noPath {
				md = strings.Replace(md, "path: "+h.repoDir+"\n", "", 1)
			}
			_, stop := startAnnotationDaemon(t, h, d, "tst-an08", md)
			defer stop()

			require.NoError(t, d.StartPlannotatorAnnotate("tst-an08"))
			require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
				3*time.Second, 20*time.Millisecond)
			h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

			got := h.waitForAnnotationRuns("tst-an08", 1, ticket.Status(tc.status))
			spawns := runs.all()
			require.Len(t, spawns, 1)

			wtPath := filepath.Join(h.wtDir, h.repoName, "tst-an08")
			switch {
			case tc.worktree:
				assert.Equal(t, wtPath, spawns[0].Dir)
				assert.Equal(t, "kontora/tst-an08", got.Branch)
			case tc.noPath:
				assert.Equal(t, h.tasksDir, spawns[0].Dir, "no repository means the ticket's own directory")
				assert.Empty(t, got.Branch)
			default:
				assert.Equal(t, h.repoDir, spawns[0].Dir, "no worktree means the repository itself")
				assert.Empty(t, got.Branch, "a annotation run must not stamp a branch")
				assert.NoDirExists(t, wtPath)
			}
			assert.NotContains(t, spawns[0].Args, "--resume", "no recorded session to continue")
			require.Len(t, got.History, 1)
			assert.False(t, got.History[0].SessionReused)
		})
	}
}

// TestAnnotate_AdoptsUninitializedTicket pins the park's side effect: a ticket
// written by something other than kontora carries no `kontora: true`, and the
// scheduler would never pick it up, so the park sets the field. The ticket keeps
// its empty pipeline and runs under the simple stage key.
func TestAnnotate_AdoptsUninitializedTicket(t *testing.T) {
	h := newPlannotatorHarness(t)
	filePath := filepath.Join(h.tasksDir, "tst-an14.md")

	var runs annotationRun
	runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	}
	d := h.newAnnotationDaemon(runner)

	md := strings.Replace(h.simpleTicketMD("tst-an14", "open", ""), "kontora: true\n", "", 1)
	_, stop := startAnnotationDaemon(t, h, d, "tst-an14", md)
	defer stop()

	before, err := ticket.ParseFile(filePath)
	require.NoError(t, err)
	require.False(t, before.Kontora, "the ticket starts unmanaged")

	require.NoError(t, d.StartPlannotatorAnnotate("tst-an14"))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

	got := h.waitForAnnotationRuns("tst-an14", 1, ticket.StatusOpen)
	assert.True(t, got.Kontora, "the park adopts the ticket")
	assert.Empty(t, got.Pipeline, "adoption must not invent a pipeline")
	assert.Empty(t, got.AnnotationReturnStatus)
	assert.Empty(t, got.Branch, "a annotation run must not stamp a branch")
	require.Len(t, got.History, 1)
	assert.Equal(t, simpleStageName, got.History[0].Stage)

	spawns := runs.all()
	require.Len(t, spawns, 1)
	assert.Contains(t, renderedPrompt(spawns[0]), "sharpen the goal")
}

// TestAnnotate_SessionReuse covers the completed-session record: a annotation run
// continues the conversation the stage's last run ended in, and receives the
// annotations rather than the interruption prompt. Every rejected record falls
// back to a fresh run without pausing the ticket.
func TestAnnotate_SessionReuse(t *testing.T) {
	const id = "tst-an09"
	const sessionID = "b2c3d4e5-0000-4000-8000-000000000002"

	cases := []struct {
		name string
		// mutate breaks one guard on an otherwise valid record. A nil mutate
		// plants no record at all.
		plant  bool
		mutate func(*resumeRecord)
		// simple makes the ticket a no-pipeline one, whose session is recorded
		// under simpleStageName rather than under a stage.
		simple bool
		want   bool
	}{
		{name: "valid record resumes", plant: true, want: true},
		{name: "a ticket with no pipeline resumes its own run", plant: true, simple: true, want: true},
		{name: "no record runs fresh"},
		{
			name:   "another stage does not resume",
			plant:  true,
			mutate: func(r *resumeRecord) { r.Stage = "step1" },
		},
		{
			name:   "another agent does not resume",
			plant:  true,
			mutate: func(r *resumeRecord) { r.Agent = agentKindPi },
		},
		{
			name:   "another instance does not resume",
			plant:  true,
			mutate: func(r *resumeRecord) { r.Instance = "other-host" },
		},
		{
			name:   "another worktree does not resume",
			plant:  true,
			mutate: func(r *resumeRecord) { r.Worktree = "/somewhere/else" },
		},
		{
			name:   "missing session file does not resume",
			plant:  true,
			mutate: func(r *resumeRecord) { r.SessionID = "c3d4e5f6-0000-4000-8000-000000000003" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPlannotatorHarness(t)
			claudeDir := t.TempDir()
			// reworkAgent falls back to the default agent, so that is the one the
			// annotation run has to be able to resume.
			h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
			h.cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}

			var runs annotationRun
			runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			}
			d := h.newAnnotationDaemon(runner)

			h.seedReviewWorktree(id)
			wtPath := filepath.Join(h.wtDir, h.repoName, id)

			frontmatter := "branch: kontora/" + id + "\n"
			stageKey := "step2"
			md := h.annotationTicketMD(id, "open", frontmatter)
			if tc.simple {
				stageKey = simpleStageName
				md = h.simpleTicketMD(id, "open", frontmatter)
			}

			if tc.plant {
				// claudeSessionFiles globs by the worktree path, so the session
				// file has to sit under the directory derived from it.
				dir := filepath.Join(claudeDir, "projects",
					strings.ReplaceAll(strings.ReplaceAll(wtPath, "/", "-"), ".", "-"))
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o644))

				rec := resumeRecord{
					SessionID: sessionID,
					Stage:     stageKey,
					Agent:     agentKindClaude,
					Worktree:  wtPath,
					Instance:  d.instanceName,
					StartedAt: time.Now().Add(-time.Minute),
				}
				if tc.mutate != nil {
					tc.mutate(&rec)
				}
				path := completedRecordPath(h.cfg, id, stageKey)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				data, err := json.Marshal(rec)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o644))
			}

			_, stop := startAnnotationDaemon(t, h, d, id, md)
			defer stop()

			require.NoError(t, d.StartPlannotatorAnnotate(id))
			require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
				3*time.Second, 20*time.Millisecond)
			h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

			got := h.waitForAnnotationRuns(id, 1, ticket.StatusOpen)
			require.Len(t, got.History, 1)
			assert.Equal(t, stageKey, got.History[0].Stage)
			assert.Equal(t, tc.want, got.History[0].SessionReused)

			spawns := runs.all()
			require.Len(t, spawns, 1, "a rejected record must not cost the run")
			prompt := renderedPrompt(spawns[0])
			assert.Contains(t, prompt, "sharpen the goal",
				"a resumed annotation run still gets the annotations")
			assert.NotContains(t, prompt, "was interrupted when the daemon restarted")

			if tc.want {
				assert.Contains(t, spawns[0].Args, "--resume")
				assert.Contains(t, spawns[0].Args, sessionID)
			} else {
				assert.NotContains(t, spawns[0].Args, "--resume")
			}
		})
	}
}

// TestAnnotate_ParkRefusedAfterTicketMoves covers the window between opening the
// annotation UI and submitting: a ticket that has since been picked up must not
// be parked, and the feedback must not be thrown away.
func TestAnnotate_ParkRefusedAfterTicketMoves(t *testing.T) {
	h := newPlannotatorHarness(t)
	d := h.newDaemonWithSpawner()

	filePath, stop := startAnnotationDaemon(t, h, d, "tst-an10",
		h.annotationTicketMD("tst-an10", "open", ""))
	defer stop()

	events, unsub := d.Subscribe()
	defer unsub()

	require.NoError(t, d.StartPlannotatorAnnotate("tst-an10"))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)

	// The ticket moves out of an editable status while the UI is open.
	tk, err := ticket.ParseFile(filePath)
	require.NoError(t, err)
	require.NoError(t, tk.SetField("status", string(ticket.StatusInProgress)))
	data, err := tk.Marshal()
	require.NoError(t, err)
	d.recordSelfWrite(filePath, data)
	require.NoError(t, os.WriteFile(filePath, data, 0o644))

	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")
	waitForPlannotatorDone(t, d, "tst-an10")

	got := h.readTask("tst-an10.md")
	assert.Equal(t, ticket.StatusInProgress, got.Status)
	assert.Empty(t, got.AnnotationReturnStatus)

	annotations := annotationsPath(h.reviewsDir, "tst-an10")
	assert.FileExists(t, annotations, "the only copy of the feedback must survive")

	ev := awaitOutcome(t, events, "tst-an10")
	assert.Equal(t, web.PlannotatorOutcomeError, ev.Outcome)
	assert.Contains(t, ev.Message, annotations, "the message names where the feedback is")
}

// TestAnnotate_ConfiguredPrompt pins that annotation_prompt replaces the built-in
// one and renders with the same helpers, and that a prompt which never asks for
// the annotations stops the run instead of spending it. An agent given no notes
// reports success, and that success is what deletes the notes.
func TestAnnotate_ConfiguredPrompt(t *testing.T) {
	cases := []struct {
		name   string
		tmpl   string
		wantOK bool
	}{
		{
			name:   "the annotations reach the agent",
			tmpl:   "CUSTOM {{ .Ticket.ID }}\n{{ plannotatorAnnotations }}",
			wantOK: true,
		},
		{
			name: "a prompt without the annotations pauses the ticket",
			tmpl: "CUSTOM {{ .Ticket.ID }}, rewrite the ticket",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const id = "tst-an11"
			h := newPlannotatorHarness(t)
			h.cfg.Agents["agent2"] = config.Agent{Binary: "claude"}
			h.cfg.AnnotationPrompt = tc.tmpl

			var runs annotationRun
			runner := func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			}
			d := h.newAnnotationDaemon(runner)

			h.seedReviewWorktree(id)
			_, stop := startAnnotationDaemon(t, h, d, id,
				h.annotationTicketMD(id, "open", "branch: kontora/"+id+"\n"))
			defer stop()

			require.NoError(t, d.StartPlannotatorAnnotate(id))
			require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
				3*time.Second, 20*time.Millisecond)
			h.stdoutCh <- annotateJSON(annotateAnnotated, "the notes")

			if !tc.wantOK {
				got := h.waitForStatus(id+".md", ticket.StatusPaused, 10*time.Second)
				assert.Contains(t, got.LastError, "does not carry the annotations")
				assert.Equal(t, ticket.StatusOpen, got.AnnotationReturnStatus,
					"the marker stays, so a fixed prompt can be retried")
				assert.FileExists(t, annotationsPath(h.reviewsDir, id))
				assert.Empty(t, runs.all(), "no agent may run without the annotations")
				assert.Empty(t, got.History)
				return
			}

			h.waitForAnnotationRuns(id, 1, ticket.StatusOpen)

			spawns := runs.all()
			require.Len(t, spawns, 1)
			prompt := renderedPrompt(spawns[0])
			assert.Contains(t, prompt, fmt.Sprintf("CUSTOM %s\nthe notes", id))
			assert.NotContains(t, prompt, "Rules for this run")
		})
	}
}

// TestAnnotate_LeavesStageSessionRecordsAlone pins that an annotation run is a
// guest in the stage's conversation: it must not touch either record, and it must
// not continue the recorded session while the stage still has an interrupted run
// to recover. Appending to that session would make its file the newest one in the
// stage's session directory, which is how the interrupted run is identified.
func TestAnnotate_LeavesStageSessionRecordsAlone(t *testing.T) {
	const id = "tst-an12"
	const crashSession = "d4e5f6a7-0000-4000-8000-000000000004"
	const doneSession = "e5f6a7b8-0000-4000-8000-000000000005"
	h := newPlannotatorHarness(t)
	claudeDir := t.TempDir()
	h.cfg.Agents["agent1"] = config.Agent{Binary: "claude"}
	h.cfg.Environment = map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}

	var runs annotationRun
	d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	})

	h.seedReviewWorktree(id)
	wtPath := filepath.Join(h.wtDir, h.repoName, id)
	// Both sessions exist on disk, so only the records decide what happens.
	sessionDir := filepath.Join(claudeDir, "projects",
		strings.ReplaceAll(strings.ReplaceAll(wtPath, "/", "-"), ".", "-"))
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	for _, s := range []string{crashSession, doneSession} {
		require.NoError(t, os.WriteFile(filepath.Join(sessionDir, s+".jsonl"), []byte("{}\n"), 0o644))
	}

	plant := func(path, sessionID string) []byte {
		data, err := json.Marshal(resumeRecord{
			SessionID: sessionID,
			Stage:     "step2",
			Agent:     agentKindClaude,
			Worktree:  wtPath,
			Instance:  d.instanceName,
			StartedAt: time.Now().Add(-time.Minute),
		})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o644))
		return data
	}
	// A run of step2 the daemon died in the middle of, and the session an earlier
	// run of step2 finished in.
	crashRecord := resumeRecordPath(h.cfg, id, "step2")
	completedRecord := completedRecordPath(h.cfg, id, "step2")
	plantedCrash := plant(crashRecord, crashSession)
	plantedDone := plant(completedRecord, doneSession)

	_, stop := startAnnotationDaemon(t, h, d, id,
		h.annotationTicketMD(id, "open", "branch: kontora/"+id+"\n"))
	defer stop()

	require.NoError(t, d.StartPlannotatorAnnotate(id))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)
	h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

	got := h.waitForAnnotationRuns(id, 1, ticket.StatusOpen)
	spawns := runs.all()
	require.Len(t, spawns, 1)
	assert.False(t, got.History[0].SessionReused,
		"the stage has an interrupted run to recover, so this one starts fresh")
	assert.NotContains(t, spawns[0].Args, "--resume")

	afterCrash, err := os.ReadFile(crashRecord)
	require.NoError(t, err, "the interrupted stage's record must survive an annotation run")
	assert.JSONEq(t, string(plantedCrash), string(afterCrash))
	afterDone, err := os.ReadFile(completedRecord)
	require.NoError(t, err)
	assert.JSONEq(t, string(plantedDone), string(afterDone),
		"an annotation run must not record its own session as the stage's")
}

// TestAnnotate_PickupWaitsForTheSession pins that a ticket is left alone while
// its annotation session is open. A stage run would edit the file the reviewer is
// reading, and the notes they then submit are refused because the ticket is
// running. The pickup is offered again when the session closes.
func TestAnnotate_PickupWaitsForTheSession(t *testing.T) {
	const id = "tst-an15"
	h := newPlannotatorHarness(t)

	var runs annotationRun
	d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
		runs.record(p)
		return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
	})

	// open is editable but never picked up, so the session opens first and the
	// ticket becomes eligible for a pickup afterwards.
	_, stop := startAnnotationDaemon(t, h, d, id, h.annotationTicketMD(id, "open", ""))
	defer stop()

	require.NoError(t, d.StartPlannotatorAnnotate(id))
	require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
		3*time.Second, 20*time.Millisecond)

	h.writeTicket(id+".md", h.annotationTicketMD(id, "todo", ""))
	require.Eventually(t, func() bool {
		info, err := d.GetTicket(id)
		return err == nil && info.Status == string(ticket.StatusTodo)
	}, 3*time.Second, 20*time.Millisecond, "the daemon should see the todo ticket")
	time.Sleep(500 * time.Millisecond)
	assert.Empty(t, runs.all(), "no stage may run while the annotation session is open")

	// Dismissed: the ticket is unchanged, so the deferred pickup has to happen.
	h.stdoutCh <- annotateJSON(annotateDismissed, "")
	waitForPlannotatorDone(t, d, id)
	require.Eventually(t, func() bool { return len(runs.all()) > 0 },
		10*time.Second, 50*time.Millisecond, "the ticket should be picked up once the session closes")
	assert.Contains(t, renderedPrompt(runs.all()[0]), "do step2 for "+id,
		"the deferred pickup runs the stage, not an annotation run")
}

// TestAnnotate_NoAnnotationsToApply covers a pickup whose annotations the run
// cannot read. Running the agent anyway would ask it to answer notes it never
// received, and its clean exit would then clear the marker as if they had been
// applied.
func TestAnnotate_NoAnnotationsToApply(t *testing.T) {
	cases := []struct {
		name         string
		returnStatus string
		// unreadable puts a directory where the annotations file belongs, which is
		// the reachable way to make the read fail.
		unreadable bool
		want       ticket.Status
		wantError  string
		wantMarker ticket.Status
	}{
		{name: "file gone, status restored", returnStatus: "human_review", want: ticket.StatusHumanReview},
		{
			// A custom status dropped from the config since the ticket was
			// annotated would take it off the board with nothing left to recover
			// it from.
			name:         "a status the config no longer has pauses instead",
			returnStatus: "blocked",
			want:         ticket.StatusPaused,
			wantError:    `"blocked" is no longer a configured status`,
		},
		{
			// Unreadable is not "nothing pending": the notes may still be there, so
			// the ticket pauses with the marker intact instead of being released.
			name:         "unreadable annotations pause the ticket",
			returnStatus: "human_review",
			unreadable:   true,
			want:         ticket.StatusPaused,
			wantError:    "reading the annotations failed",
			wantMarker:   "human_review",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const id = "tst-an13"
			h := newPlannotatorHarness(t)

			var runs annotationRun
			d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			})

			if tc.unreadable {
				require.NoError(t, os.MkdirAll(annotationsPath(h.reviewsDir, id), 0o755))
			}

			// Parked for an annotation run, but the annotations never arrive.
			_, stop := startAnnotationDaemon(t, h, d, id,
				h.annotationTicketMD(id, "todo", "annotation_return_status: "+tc.returnStatus+"\n"))
			defer stop()

			got := h.waitForStatus(id+".md", tc.want, 10*time.Second)
			assert.Equal(t, tc.wantMarker, got.AnnotationReturnStatus)
			assert.Empty(t, got.History)
			assert.Empty(t, runs.all(), "no agent runs without annotations")
			if tc.wantError != "" {
				assert.Contains(t, got.LastError, tc.wantError)
			}
		})
	}
}

// TestAnnotate_PiSessionDir pins where a annotation run puts a pi session. A fresh
// one goes to a directory of its own, because a pi session file is resolved by
// mtime inside the stage's directory and a new annotation session there would become
// what the stage's crash recovery resumes into. A resumed one appends to the
// stage's own file, so its directory is the stage's: that is where the log is
// materialized from.
func TestAnnotate_PiSessionDir(t *testing.T) {
	cases := []struct {
		name   string
		resume bool
	}{
		{name: "fresh session stays out of the stage directory"},
		{name: "resumed session reads the stage directory", resume: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const id = "tst-an14"
			h := newPlannotatorHarness(t)
			h.cfg.Agents["agent1"] = config.Agent{Binary: "pi"}

			var runs annotationRun
			d := h.newAnnotationDaemon(func(_ context.Context, p RunnerParams) (process.Result, error) {
				runs.record(p)
				return process.Result{ExitCode: 0, StartedAt: time.Now(), ExitedAt: time.Now()}, nil
			})

			h.seedReviewWorktree(id)
			stageDir := piSessionDir(h.cfg, id, "step2")
			if tc.resume {
				// The session step2's last run left behind, and the record naming it.
				require.NoError(t, os.MkdirAll(stageDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(stageDir, "session.jsonl"), []byte("{}\n"), 0o644))
				rec, err := json.Marshal(resumeRecord{
					Stage:     "step2",
					Agent:     agentKindPi,
					Worktree:  filepath.Join(h.wtDir, h.repoName, id),
					Instance:  d.instanceName,
					StartedAt: time.Now().Add(-time.Minute),
				})
				require.NoError(t, err)
				path := completedRecordPath(h.cfg, id, "step2")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, rec, 0o644))
			}

			_, stop := startAnnotationDaemon(t, h, d, id,
				h.annotationTicketMD(id, "open", "branch: kontora/"+id+"\n"))
			defer stop()

			require.NoError(t, d.StartPlannotatorAnnotate(id))
			require.Eventually(t, func() bool { return h.callCount.Load() == 1 },
				3*time.Second, 20*time.Millisecond)
			h.stdoutCh <- annotateJSON(annotateAnnotated, "sharpen the goal")

			got := h.waitForAnnotationRuns(id, 1, ticket.StatusOpen)
			spawns := runs.all()
			require.Len(t, spawns, 1)
			assert.Equal(t, tc.resume, got.History[0].SessionReused)

			if tc.resume {
				assert.Equal(t, stageDir, spawns[0].SessionDir)
				assert.Contains(t, spawns[0].Args, filepath.Join(stageDir, "session.jsonl"))
			} else {
				assert.Equal(t, piSessionDir(h.cfg, id, "step2-annotation"), spawns[0].SessionDir)
				assert.NotEqual(t, stageDir, spawns[0].SessionDir)
			}
		})
	}
}
