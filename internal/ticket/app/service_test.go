package app

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

// memRepo is an in-memory Repository for tests.
type memRepo struct {
	tickets map[string]*StoredTicket
	saveErr error // when set, Save returns it without mutating state
}

func newMemRepo() *memRepo {
	return &memRepo{tickets: make(map[string]*StoredTicket)}
}

func (r *memRepo) add(id, content string) {
	t, err := ticket.ParseBytes([]byte(content))
	if err != nil {
		panic(fmt.Sprintf("memRepo.add: %v", err))
	}
	r.tickets[id] = &StoredTicket{Ticket: t, FilePath: "/tmp/" + id + ".md"}
}

func (r *memRepo) Resolve(idOrPrefix string) (string, error) {
	if _, ok := r.tickets[idOrPrefix]; ok {
		return idOrPrefix, nil
	}
	for id := range r.tickets {
		if len(idOrPrefix) <= len(id) && id[:len(idOrPrefix)] == idOrPrefix {
			return id, nil
		}
	}
	return "", fmt.Errorf("ticket %q not found", idOrPrefix)
}

func (r *memRepo) Get(id string) (*StoredTicket, error) {
	st, ok := r.tickets[id]
	if !ok {
		return nil, fmt.Errorf("ticket %q not found", id)
	}
	return st, nil
}

func (r *memRepo) List() ([]*StoredTicket, error) {
	out := make([]*StoredTicket, 0, len(r.tickets))
	for _, st := range r.tickets {
		out = append(out, st)
	}
	return out, nil
}

func (r *memRepo) Save(st *StoredTicket) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	r.tickets[st.Ticket.ID] = st
	return nil
}

// spyRuntime records calls to RuntimeHooks methods.
type spyRuntime struct {
	enqueued  []string
	cancelled []string
	updated   []string
	deleted   []string
}

func (s *spyRuntime) Enqueue(t *ticket.Ticket)   { s.enqueued = append(s.enqueued, t.ID) }
func (s *spyRuntime) Cancel(id string)           { s.cancelled = append(s.cancelled, id) }
func (s *spyRuntime) BroadcastUpdated(id string) { s.updated = append(s.updated, id) }
func (s *spyRuntime) BroadcastDeleted(id string) { s.deleted = append(s.deleted, id) }

func testCfg() *config.Config {
	return &config.Config{
		TicketsDir:   "/tmp/tickets",
		DefaultAgent: "claude-sonnet",
		Agents: map[string]config.Agent{
			"claude-sonnet": {Binary: "claude"},
		},
		Stages: map[string]config.Stage{
			"code":   {Prompt: "code"},
			"review": {Prompt: "review"},
		},
		Pipelines: map[string]config.Pipeline{
			"default": {
				{Stage: "code", Agent: "claude-sonnet", OnSuccess: "next", OnFailure: "pause"},
				{Stage: "review", Agent: "claude-sonnet", OnSuccess: "done", OnFailure: "pause"},
			},
		},
	}
}

func TestSetStatus_Done_SetsCompletedAt(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\npipeline: default\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.SetStatus("tst-001", ticket.StatusDone)
	require.NoError(t, err)
	assert.Equal(t, "done", result.Status)
	assert.NotNil(t, repo.tickets["tst-001"].Ticket.CompletedAt)
	assert.Equal(t, []string{"tst-001"}, rt.updated)
}

func TestSetStatus_CancelsRunningAgent(t *testing.T) {
	cases := []struct {
		name   string
		status ticket.Status
	}{
		{"open", ticket.StatusOpen},
		{"cancelled", ticket.StatusCancelled},
		{"done", ticket.StatusDone},
		{"human_review", ticket.StatusHumanReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("tst-001", "---\nid: tst-001\nstatus: in_progress\nkontora: true\npipeline: default\nstage: code\n---\n# Test\n")
			rt := &spyRuntime{}
			svc := New(Static(testCfg()), repo, rt)

			_, err := svc.SetStatus("tst-001", tc.status)
			require.NoError(t, err)
			assert.Equal(t, []string{"tst-001"}, rt.cancelled)
		})
	}
}

// A pending annotation run restores the status the ticket had before it was
// parked, so a user who moves the ticket somewhere else has to cancel it.
func TestSetStatus_ClearsPendingAnnotation(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\npipeline: default\nannotation_return_status: human_review\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.SetStatus("tst-001", ticket.StatusPaused)
	require.NoError(t, err)
	assert.Empty(t, repo.tickets["tst-001"].Ticket.AnnotationReturnStatus)
}

func TestSetStatus_AlreadySame(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.SetStatus("tst-001", ticket.StatusTodo)
	require.ErrorIs(t, err, ErrInvalidState)
	require.ErrorContains(t, err, "already todo")
}

func TestSetStatus_InvalidStatus(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.SetStatus("tst-001", ticket.Status("bogus"))
	require.ErrorContains(t, err, "invalid status")
}

func TestRetry_ResetsAttempt(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: paused\nkontora: true\nattempt: 3\npipeline: default\nstage: code\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.Retry("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "todo", result.Status)
	assert.Equal(t, 0, repo.tickets["tst-001"].Ticket.Attempt)
	assert.Equal(t, []string{"tst-001"}, rt.enqueued)
}

func TestRetry_ClearsLastError(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: paused\nkontora: true\nattempt: 1\npipeline: default\nstage: code\nlast_error: \"agent exited with code 1\"\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	_, err := svc.Retry("tst-001")
	require.NoError(t, err)
	assert.Empty(t, repo.tickets["tst-001"].Ticket.LastError)
}

func TestRetry_RejectsInProgress(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: in_progress\nkontora: true\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Retry("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestRetry_RejectsTodo(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Retry("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestRetry_RejectsNonKontora(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: paused\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	_, err := svc.Retry("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
	assert.Empty(t, rt.enqueued)
	assert.Equal(t, ticket.StatusPaused, repo.tickets["tst-001"].Ticket.Status)
}

func TestRetry_RejectsArchived(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: archived\nkontora: true\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	_, err := svc.Retry("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
	assert.Empty(t, rt.enqueued)
	assert.Equal(t, ticket.StatusArchived, repo.tickets["tst-001"].Ticket.Status)
}

func TestSkip_AdvancesToNextStage(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: in_progress\nkontora: true\npipeline: default\nstage: code\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.Skip("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "todo", result.Status)
	assert.Equal(t, "review", repo.tickets["tst-001"].Ticket.Stage)
	assert.Equal(t, 0, repo.tickets["tst-001"].Ticket.Attempt)
	assert.Equal(t, []string{"tst-001"}, rt.enqueued)
}

func TestSkip_LastStage_MarksDone(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: in_progress\nkontora: true\npipeline: default\nstage: review\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.Skip("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "done", result.Status)
	assert.NotNil(t, repo.tickets["tst-001"].Ticket.CompletedAt, "skip to done must set completed_at")
	assert.Empty(t, rt.enqueued)
}

func TestSkip_UnknownPipeline(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: in_progress\nkontora: true\npipeline: nonexistent\nstage: code\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Skip("tst-001")
	require.ErrorContains(t, err, "unknown pipeline")
}

func TestSkip_RejectsNonKontora(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: paused\npipeline: default\nstage: code\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	_, err := svc.Skip("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
	assert.Empty(t, rt.enqueued)
	assert.Equal(t, "code", repo.tickets["tst-001"].Ticket.Stage)
}

func TestSkip_RejectsArchived(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: archived\nkontora: true\npipeline: default\nstage: code\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	_, err := svc.Skip("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
	assert.Empty(t, rt.enqueued)
	assert.Equal(t, ticket.StatusArchived, repo.tickets["tst-001"].Ticket.Status)
}

func TestInit_SetsAllFields(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: open\npath: ~/projects/test\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.Init("tst-001", InitRequest{
		Pipeline: "default",
		Path:     "~/projects/test",
	})
	require.NoError(t, err)
	assert.Equal(t, "todo", result.Status)

	tkt := repo.tickets["tst-001"].Ticket
	assert.True(t, tkt.Kontora)
	assert.Equal(t, "default", tkt.Pipeline)
	assert.Equal(t, "code", tkt.Stage, "should default to first pipeline stage")
	assert.Equal(t, 0, tkt.Attempt)
	assert.Equal(t, []string{"tst-001"}, rt.enqueued)
}

func TestInit_AlreadyInitialized(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\npipeline: default\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Init("tst-001", InitRequest{Pipeline: "default"})
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestInit_UnknownAgent(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: open\npath: ~/projects/test\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Init("tst-001", InitRequest{
		Pipeline: "default",
		Agent:    "nonexistent",
	})
	require.ErrorContains(t, err, "unknown agent")
}

func TestInit_CustomStage(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: open\npath: ~/projects/test\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Init("tst-001", InitRequest{
		Pipeline: "default",
		Stage:    "review",
	})
	require.NoError(t, err)
	assert.Equal(t, "review", repo.tickets["tst-001"].Ticket.Stage)
}

func TestInit_InvalidStatus(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: open\npath: ~/projects/test\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Init("tst-001", InitRequest{
		Pipeline: "default",
		Status:   "done",
	})
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestGet_IncludesBody(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\npipeline: default\nstage: code\n---\n# Hello world\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	v, err := svc.Get("tst-001", GetOptions{IncludeBody: true})
	require.NoError(t, err)
	assert.Equal(t, "Hello world", v.Title)
	assert.Contains(t, v.Body, "# Hello world")
}

func TestGet_ExcludesBody(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Hello world\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	v, err := svc.Get("tst-001", GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Hello world", v.Title)
	assert.Empty(t, v.Body)
}

func TestGet_PrefixResolve(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	v, err := svc.Get("tst", GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "tst-001", v.ID)
}

func TestList_IncludesKontoraAndNonKontoraTickets(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\n---\n# Kontora ticket\n")
	repo.add("tst-002", "---\nid: tst-002\nstatus: todo\n---\n# Not kontora\n")
	repo.add("notes", "---\ntitle: Just some notes\n---\n# Notes\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	views, err := svc.List(ListOptions{})
	require.NoError(t, err)
	ids := make([]string, 0, len(views))
	for _, v := range views {
		ids = append(ids, v.ID)
	}
	assert.ElementsMatch(t, []string{"tst-001", "tst-002"}, ids)
}

func TestList_OpenNonKontoraIncluded(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: open\n---\n# Open non-kontora\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	views, err := svc.List(ListOptions{})
	require.NoError(t, err)
	assert.Len(t, views, 1)
	assert.False(t, views[0].Kontora)
}

// TestBuildView_Summaries: the per-run and ticket-level summaries are detail
// fields. The board list projection carries neither.
func TestBuildView_Summaries(t *testing.T) {
	tkt, err := ticket.ParseBytes([]byte("---\nid: tst-001\nstatus: done\nkontora: true\nsummary: the last run\nfinal_summary: the whole ticket\n---\n# Test\n"))
	require.NoError(t, err)

	detail := BuildView(testCfg(), tkt, true)
	assert.Equal(t, "the last run", detail.Summary)
	assert.Equal(t, "the whole ticket", detail.FinalSummary)

	list := BuildView(testCfg(), tkt, false)
	assert.Empty(t, list.Summary)
	assert.Empty(t, list.FinalSummary)
}

func TestBuildView_AgentResolution(t *testing.T) {
	cases := []struct {
		name         string
		ticket       string
		wantAgent    string
		wantOverride bool
	}{
		{
			name:         "from pipeline stage",
			ticket:       "---\nid: t1\nstatus: todo\nkontora: true\npipeline: default\nstage: code\n---\n# T\n",
			wantAgent:    "claude-sonnet",
			wantOverride: false,
		},
		{
			name:         "ticket-level override",
			ticket:       "---\nid: t1\nstatus: todo\nkontora: true\npipeline: default\nstage: code\nagent: opus\n---\n# T\n",
			wantAgent:    "opus",
			wantOverride: true,
		},
		{
			name:         "default agent for simple kontora",
			ticket:       "---\nid: t1\nstatus: todo\nkontora: true\n---\n# T\n",
			wantAgent:    "claude-sonnet",
			wantOverride: false,
		},
	}

	cfg := testCfg()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ticket.ParseBytes([]byte(tc.ticket))
			require.NoError(t, err)
			v := BuildView(cfg, tkt, false)
			assert.Equal(t, tc.wantAgent, v.Agent)
			assert.Equal(t, tc.wantOverride, v.AgentOverride)
		})
	}
}

func TestBuildView_Stages(t *testing.T) {
	cases := []struct {
		name       string
		ticket     string
		wantStages []string
	}{
		{
			name:       "pipeline stages",
			ticket:     "---\nid: t1\nstatus: todo\nkontora: true\npipeline: default\nstage: code\n---\n# T\n",
			wantStages: []string{"code", "review"},
		},
		{
			name:       "simple kontora default stage",
			ticket:     "---\nid: t1\nstatus: todo\nkontora: true\n---\n# T\n",
			wantStages: []string{"default"},
		},
		{
			name:       "non-kontora no stages",
			ticket:     "---\nid: t1\nstatus: open\n---\n# T\n",
			wantStages: nil,
		},
	}

	cfg := testCfg()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tkt, err := ticket.ParseBytes([]byte(tc.ticket))
			require.NoError(t, err)
			v := BuildView(cfg, tkt, false)
			assert.Equal(t, tc.wantStages, v.Stages)
		})
	}
}

func TestBuildView_ClaimedBy(t *testing.T) {
	cfg := testCfg()
	tkt, err := ticket.ParseBytes([]byte(
		"---\nid: t1\nstatus: in_progress\nkontora: true\npipeline: default\nstage: code\nclaimed_by: alpha\n---\n# T\n"))
	require.NoError(t, err)

	// ClaimedBy is populated in both the list and detail projections.
	assert.Equal(t, "alpha", BuildView(cfg, tkt, false).ClaimedBy)
	assert.Equal(t, "alpha", BuildView(cfg, tkt, true).ClaimedBy)
}

func TestRun_FromOpen(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: open\nkontora: true\npipeline: default\nstage: code\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.Run("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "todo", result.Status)
	assert.Equal(t, ticket.StatusTodo, repo.tickets["tst-001"].Ticket.Status)
	assert.Equal(t, []string{"tst-001"}, rt.enqueued)
}

func TestRun_FromTodo(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\npipeline: default\nstage: code\n---\n# Test\n")
	rt := &spyRuntime{}
	svc := New(Static(testCfg()), repo, rt)

	result, err := svc.Run("tst-001")
	require.NoError(t, err)
	assert.Equal(t, "todo", result.Status)
	assert.Equal(t, []string{"tst-001"}, rt.enqueued)
}

func TestRun_RejectsInProgress(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: in_progress\nkontora: true\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Run("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestRun_RejectsNonKontora(t *testing.T) {
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\n---\n# Test\n")
	svc := New(Static(testCfg()), repo, &spyRuntime{})

	_, err := svc.Run("tst-001")
	require.ErrorIs(t, err, ErrInvalidState)
}

func TestAgentForStage(t *testing.T) {
	cfg := testCfg()

	assert.Equal(t, "claude-sonnet", AgentForStage(cfg, "default", "code"))
	assert.Equal(t, "claude-sonnet", AgentForStage(cfg, "default", "review"))
	assert.Equal(t, "", AgentForStage(cfg, "default", "nonexistent"))
	assert.Equal(t, "", AgentForStage(cfg, "nonexistent", "code"))
	assert.Equal(t, "", AgentForStage(cfg, "default", ""))
}

// TestService_LiveConfig verifies one Service instance follows the config its
// ConfigFunc returns, so a daemon reload reaches the service without the
// service being rebuilt.
func TestService_LiveConfig(t *testing.T) {
	cfg := testCfg()
	repo := newMemRepo()
	repo.add("tst-001", "---\nid: tst-001\nstatus: todo\nkontora: true\npipeline: default\n---\n# Test\n")
	svc := New(func() *config.Config { return cfg }, repo, &spyRuntime{})

	_, err := svc.SetStatus("tst-001", ticket.Status("waiting_external"))
	require.Error(t, err, "an unknown custom status must be rejected before the reload")

	// Agents and pipelines.
	repo.add("tst-002", "---\nid: tst-002\nstatus: open\npath: ~/p\n---\n# Test\n")
	_, err = svc.Init("tst-002", InitRequest{Pipeline: "verify-only", Path: "~/p"})
	require.ErrorIs(t, err, ErrInvalidState, "an unknown pipeline must be rejected before the reload")
	_, err = svc.Init("tst-002", InitRequest{Pipeline: "default", Path: "~/p", Agent: "codex"})
	require.ErrorIs(t, err, ErrUnknownAgent, "an unknown agent must be rejected before the reload")

	// Swap the config the same instance reads.
	next := testCfg()
	next.Statuses = []string{"waiting_external"}
	next.Agents["codex"] = config.Agent{Binary: "codex"}
	next.Stages["verify"] = config.Stage{Prompt: "verify"}
	next.Pipelines["verify-only"] = config.Pipeline{
		{Stage: "verify", Agent: "codex", OnSuccess: "done", OnFailure: "pause"},
	}
	cfg = next

	_, err = svc.SetStatus("tst-001", ticket.Status("waiting_external"))
	require.NoError(t, err, "the reloaded custom status must be accepted")

	_, err = svc.Init("tst-002", InitRequest{Pipeline: "verify-only", Path: "~/p", Agent: "codex"})
	require.NoError(t, err, "the reloaded pipeline and agent must be accepted")
	assert.Equal(t, "verify", repo.tickets["tst-002"].Ticket.Stage,
		"init must use the reloaded pipeline's first stage")
}

func TestInit_ProjectDefaults(t *testing.T) {
	cases := []struct {
		name         string
		frontmatter  string // extra ticket fields, one per line
		req          InitRequest
		wantPipeline string
		wantAgent    string
		wantStage    string
		wantBranch   string
	}{
		{
			name:         "branch is written when given",
			req:          InitRequest{Path: "~/projects/test", Branch: "feat/login"},
			wantPipeline: "default",
			wantAgent:    "claude-sonnet",
			wantStage:    "code",
			wantBranch:   "feat/login",
		},
		{
			name:         "a blank branch keeps the one the ticket carries",
			frontmatter:  "branch: kontora/tst-001\n",
			req:          InitRequest{Path: "~/projects/test"},
			wantPipeline: "default",
			wantAgent:    "claude-sonnet",
			wantStage:    "code",
			wantBranch:   "kontora/tst-001",
		},
		{
			name:         "blank fields take project defaults",
			req:          InitRequest{Path: "~/projects/test"},
			wantPipeline: "default",
			wantAgent:    "claude-sonnet",
			wantStage:    "code",
		},
		{
			name:         "the ticket's own values win over project defaults",
			frontmatter:  "pipeline: release\nagent: codex\n",
			req:          InitRequest{Path: "~/projects/test"},
			wantPipeline: "release",
			wantAgent:    "codex",
			wantStage:    "code",
		},
		{
			name:         "none clears an agent the ticket already carries",
			frontmatter:  "agent: codex\n",
			req:          InitRequest{Path: "~/projects/test", Agent: "none"},
			wantPipeline: "default",
			wantStage:    "code",
		},
		{
			name:         "explicit values win",
			req:          InitRequest{Path: "~/projects/test", Pipeline: "release", Agent: "codex"},
			wantPipeline: "release",
			wantAgent:    "codex",
			wantStage:    "code",
		},
		{
			name:      "none opts the pipeline out and keeps the project agent",
			req:       InitRequest{Path: "~/projects/test", Pipeline: "none"},
			wantAgent: "claude-sonnet",
		},
		{
			name:         "path with no project entry stays blank",
			req:          InitRequest{Path: "~/projects/other"},
			wantPipeline: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.Pipelines["release"] = cfg.Pipelines["default"]
			cfg.Agents["codex"] = config.Agent{Binary: "codex"}
			cfg.Projects = map[string]config.Project{
				"test": {Path: "~/projects/test", Pipeline: "default", Agent: "claude-sonnet"},
			}

			repo := newMemRepo()
			repo.add("tst-001", "---\nid: tst-001\nstatus: open\n"+tc.frontmatter+"---\n# Test\n")
			svc := New(Static(cfg), repo, &spyRuntime{})

			_, err := svc.Init("tst-001", tc.req)
			require.NoError(t, err)

			tkt := repo.tickets["tst-001"].Ticket
			assert.Equal(t, tc.wantPipeline, tkt.Pipeline)
			assert.Equal(t, tc.wantAgent, tkt.Agent)
			assert.Equal(t, tc.wantStage, tkt.Stage)
			assert.Equal(t, tc.wantBranch, tkt.Branch)
		})
	}
}
