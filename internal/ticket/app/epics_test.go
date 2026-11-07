package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/testutil"
	"github.com/worksonmyai/kontora/internal/ticket"
)

func TestEpicActivation(t *testing.T) {
	path := testutil.InitRepo(t)
	other := testutil.InitRepo(t)
	for _, tc := range []struct {
		name, mode, childPath string
		base                  string
		req                   InitRequest
		wantErr               bool
	}{
		{name: "passive requires explicit start", req: InitRequest{}, wantErr: true},
		{name: "open does not activate", req: InitRequest{Status: "open"}, wantErr: true},
		{name: "explicit start", req: InitRequest{Status: "todo"}},
		{name: "marked draft", mode: "agent", req: InitRequest{Status: "open"}},
		{name: "marked start", mode: "agent", req: InitRequest{}},
		{name: "pipeline opt out", req: InitRequest{Status: "todo", Pipeline: "none"}},
		{name: "pipeline rejected", req: InitRequest{Status: "todo", Pipeline: "default"}, wantErr: true},
		{name: "stage rejected", req: InitRequest{Status: "todo", Stage: "code"}, wantErr: true},
		{name: "unknown agent", req: InitRequest{Status: "todo", Agent: "missing"}, wantErr: true},
		{name: "invalid branch", req: InitRequest{Status: "todo", Branch: "bad..branch"}, wantErr: true},
		{name: "valid branch", req: InitRequest{Status: "todo", Branch: "epic/integration"}},
		{name: "missing base branch", base: "missing-base", req: InitRequest{Status: "todo"}, wantErr: true},
		{name: "child integration branch collision", childPath: path, req: InitRequest{Status: "todo", Branch: "child-work"}, wantErr: true},
		{name: "same repo child", childPath: path, req: InitRequest{Status: "todo"}},
		{name: "cross repo child", childPath: other, req: InitRequest{Status: "todo"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epc-100", fmt.Sprintf("---\nid: epc-100\nkind: epic\nkontora: true\nstatus: open\nepic_execution: %q\npath: %q\n---\n# Epic\n", tc.mode, path))
			require.NoError(t, repo.tickets["epc-100"].Ticket.SetField("base_branch", tc.base))
			if tc.childPath != "" {
				repo.add("child", fmt.Sprintf("---\nid: child\nparent: epc-100\npath: %q\npipeline: default\nstatus: open\nbranch: child-work\n---\n# Child\n", tc.childPath))
			}
			cfg := testCfg()
			cfg.DefaultPipeline = "default" // never inherited by the supervisor
			cfg.Pipelines["child"] = cfg.Pipelines["default"]
			cfg.Projects = map[string]config.Project{
				"repo": {Path: path, Pipeline: "default", EpicChildPipeline: "child", Agent: cfg.DefaultAgent},
			}
			runtime := &spyRuntime{}
			svc := New(Static(cfg), repo, runtime)
			_, err := svc.Init("epc-100", tc.req)
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, repo.saved)
				assert.Empty(t, runtime.enqueued)
				return
			}
			require.NoError(t, err)
			epic := repo.tickets["epc-100"].Ticket
			assert.True(t, epic.IsAgentEpic())
			assert.Empty(t, epic.Pipeline)
			assert.Empty(t, epic.Stage)
			assert.Equal(t, cfg.DefaultAgent, epic.Agent)
			assert.Equal(t, []string{"epc-100"}, repo.saved)
			if tc.req.Status != "open" {
				assert.Equal(t, []string{"epc-100"}, runtime.enqueued)
			}
			if tc.childPath != "" {
				assert.Equal(t, ticket.StatusOpen, repo.tickets["child"].Ticket.Status)
				assert.Equal(t, "default", repo.tickets["child"].Ticket.Pipeline)
			}
		})
	}
}

func TestExecutionBranchSeparationIncludesGeneratedNames(t *testing.T) {
	path := testutil.InitRepo(t)
	for _, tc := range []struct {
		name, mode, epicBranch, childBranch string
	}{
		{name: "child generated slug", mode: "slug", epicBranch: "work/child-child"},
		{name: "epic generated slug", mode: "slug", childBranch: "work/epic-epc-100"},
		{name: "child generated id", mode: "off", epicBranch: "work/child"},
		{name: "epic generated id", mode: "off", childBranch: "work/epc-100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epc-100", fmt.Sprintf("---\nid: epc-100\nkind: epic\nepic_execution: agent\nstatus: open\npath: %q\nbranch: %q\n---\n# Epic\n", path, tc.epicBranch))
			repo.add("child", fmt.Sprintf("---\nid: child\nparent: epc-100\nstatus: open\npath: %q\nbranch: %q\n---\n# Child\n", path, tc.childBranch))
			cfg := testCfg()
			cfg.BranchPrefix = "work"
			cfg.BranchNaming.Mode = tc.mode
			svc := New(Static(cfg), repo, &spyRuntime{})
			for _, id := range []string{"epc-100", "child"} {
				require.ErrorIs(t, svc.ValidateExecutionSettings(repo.tickets[id].Ticket), ErrInvalidState)
			}
			_, err := svc.Init("epc-100", InitRequest{Status: "todo"})
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Empty(t, repo.saved)
		})
	}
}

func TestInitChildExecutionSettings(t *testing.T) {
	path := testutil.InitRepo(t)
	other := testutil.InitRepo(t)
	for _, tc := range []struct {
		name, parentMode, path, branch string
		childPipeline                  string
		missingParent                  bool
		wantErr                        bool
	}{
		{name: "same repository separate branch", parentMode: "agent", path: path, branch: "child-work"},
		{name: "cross repository", parentMode: "agent", path: other, wantErr: true},
		{name: "shared integration branch", parentMode: "agent", path: path, branch: "integration", wantErr: true},
		{name: "passive grouping parent", path: other, branch: "integration"},
		{name: "dangling grouping parent", path: other, missingParent: true},
		{name: "active child default", parentMode: "agent", path: path, childPipeline: "child"},
		{name: "passive child default", path: other, childPipeline: "child"},
		{name: "missing parent with child default", path: other, missingParent: true, childPipeline: "child"},
	} {
		for _, status := range []string{"open", "todo"} {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				repo := newMemRepo()
				if !tc.missingParent {
					repo.add("epc-100", fmt.Sprintf("---\nid: epc-100\nkind: epic\nepic_execution: %q\npath: %q\nbranch: integration\n---\n# Epic\n", tc.parentMode, path))
				}
				repo.add("child", fmt.Sprintf("---\nid: child\nparent: epc-100\nstatus: open\npath: %q\n---\n# Child\n", path))
				runtime := &spyRuntime{}
				cfg := testCfg()
				cfg.TicketsDir = t.TempDir()
				cfg.Pipelines["child"] = cfg.Pipelines["default"]
				cfg.Projects = map[string]config.Project{
					"repo": {Path: tc.path, Pipeline: "default", EpicChildPipeline: tc.childPipeline},
				}
				svc := New(Static(cfg), repo, runtime)
				_, err := svc.Init("child", InitRequest{Path: tc.path, Branch: tc.branch, Status: status})
				if tc.wantErr {
					require.ErrorIs(t, err, ErrInvalidState)
					assert.Empty(t, repo.saved)
					assert.Empty(t, runtime.enqueued)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, tc.path, repo.tickets["child"].Ticket.Path)
				assert.Equal(t, tc.branch, repo.tickets["child"].Ticket.Branch)
				assert.Equal(t, ticket.Status(status), repo.tickets["child"].Ticket.Status)
				wantPipeline := "default"
				if tc.childPipeline != "" && !tc.missingParent {
					wantPipeline = tc.childPipeline
				}
				assert.Equal(t, wantPipeline, repo.tickets["child"].Ticket.Pipeline)
			})
		}
	}
}

type parentReadRepo struct {
	*memRepo
	parentID string
	getErr   error
	reads    int
}

func (r *parentReadRepo) WithMutation(fn func(MutationRepository) error) error {
	return r.memRepo.WithMutation(func(MutationRepository) error { return fn(r) })
}

func (r *parentReadRepo) Get(id string) (*StoredTicket, error) {
	if id == r.parentID {
		r.reads++
		if r.getErr != nil {
			return nil, r.getErr
		}
	}
	return r.memRepo.Get(id)
}

func TestInitParentDefaultsResolution(t *testing.T) {
	path := testutil.InitRepo(t)
	for _, tc := range []struct {
		name, parentID, disk, cached, pipeline, stored string
		readErr                                        error
		unset, wantErr                                 bool
		wantPipeline                                   string
		wantReads                                      int
	}{
		{name: "uncached passive epic", disk: "epic", wantPipeline: "child", wantReads: 1},
		{name: "uncached active epic", disk: "agent", wantPipeline: "child", wantReads: 1},
		{name: "uncached ordinary parent", disk: "ordinary", wantPipeline: "default", wantReads: 1},
		{name: "missing parent", wantPipeline: "default", wantReads: 1},
		{name: "filesystem missing from repository", readErr: os.ErrNotExist, disk: "epic", wantPipeline: "child", wantReads: 1},
		{name: "repository error cannot use disk fallback", readErr: os.ErrPermission, disk: "epic", wantErr: true, wantReads: 1},
		{name: "disk parse error", disk: "malformed", wantErr: true, wantReads: 1},
		{name: "disk read error", disk: "directory", wantErr: true, wantReads: 1},
		{name: "disk id mismatch", disk: "mismatch", wantErr: true, wantReads: 1},
		{name: "repository epic", cached: "epic", wantPipeline: "child", wantReads: 2},
		{name: "repository ordinary parent", cached: "ordinary", wantPipeline: "default", wantReads: 2},
		{name: "repository epic id mismatch", cached: "mismatch", wantErr: true, wantReads: 1},
		{name: "repository ordinary id mismatch", cached: "ordinary-mismatch", wantErr: true, wantReads: 1},
		{name: "unsafe parent", parentID: "../outside", wantErr: true, wantReads: 1},
		{name: "no parent", parentID: "none", wantPipeline: "default"},
		{name: "named request skips lookup", readErr: os.ErrPermission, pipeline: "default", wantPipeline: "default"},
		{name: "stored pipeline skips lookup", readErr: os.ErrPermission, stored: "default", wantPipeline: "default"},
		{name: "none request skips lookup", readErr: os.ErrPermission, pipeline: "none"},
		{name: "stored none skips lookup", readErr: os.ErrPermission, stored: "none"},
		{name: "unset child default skips lookup", readErr: os.ErrPermission, unset: true, wantPipeline: "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.TicketsDir = t.TempDir()
			cfg.Pipelines["child"] = cfg.Pipelines["default"]
			project := config.Project{Path: path, Pipeline: "default", EpicChildPipeline: "child", Agent: cfg.DefaultAgent}
			if tc.unset {
				project.EpicChildPipeline = ""
			}
			cfg.Projects = map[string]config.Project{"repo": project}
			parentID := tc.parentID
			switch parentID {
			case "":
				parentID = "epc-100"
			case "none":
				parentID = ""
			}
			repo := &parentReadRepo{memRepo: newMemRepo(), parentID: parentID, getErr: tc.readErr}
			repo.add("child", fmt.Sprintf("---\nid: child\nstatus: open\nparent: %q\npath: %q\npipeline: %q\n---\n# Child\n", parentID, path, tc.stored))
			if tc.cached != "" {
				id, kind := parentID, "epic"
				if strings.HasSuffix(tc.cached, "mismatch") {
					id = "other"
				}
				if strings.HasPrefix(tc.cached, "ordinary") {
					kind = ""
				}
				repo.add(parentID, fmt.Sprintf("---\nid: %s\nkind: %q\n---\n# Parent\n", id, kind))
			}
			if tc.disk != "" {
				file := filepath.Join(cfg.TicketsDir, parentID+".md")
				kind, mode, id := "epic", "", parentID
				switch tc.disk {
				case "agent":
					mode = "agent"
				case "ordinary":
					kind = ""
				case "mismatch":
					id = "other"
				}
				content := fmt.Sprintf("---\nid: %s\nkind: %q\nepic_execution: %q\npath: %q\n---\n# Parent\n", id, kind, mode, path)
				if tc.disk == "malformed" {
					content = "---\nid: [\n---\n"
				}
				if tc.disk == "directory" {
					require.NoError(t, os.Mkdir(file, 0o755))
				} else {
					require.NoError(t, os.WriteFile(file, []byte(content), 0o644))
				}
			}
			before, err := repo.tickets["child"].Ticket.Marshal()
			require.NoError(t, err)
			runtime := &spyRuntime{}
			svc := New(Static(cfg), repo, runtime)
			_, err = svc.Init("child", InitRequest{Pipeline: tc.pipeline, Status: "open"})
			if tc.wantReads == 0 {
				assert.Zero(t, repo.reads)
			} else {
				// Init resolves once during preparation and again in the scope.
				assert.GreaterOrEqual(t, repo.reads, tc.wantReads)
			}
			if tc.wantErr {
				require.Error(t, err)
				if tc.readErr != nil {
					assert.ErrorIs(t, err, tc.readErr)
				}
				assert.Empty(t, repo.saved)
				assert.Empty(t, runtime.updated)
				after, marshalErr := repo.tickets["child"].Ticket.Marshal()
				require.NoError(t, marshalErr)
				assert.Equal(t, before, after)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantPipeline, repo.tickets["child"].Ticket.Pipeline)
			assert.Equal(t, cfg.DefaultAgent, repo.tickets["child"].Ticket.Agent)
			assert.Equal(t, []string{"child"}, repo.saved)
		})
	}
}

type staleListingRepo struct {
	*memRepo
	listed []*StoredTicket
}

func (r staleListingRepo) List() ([]*StoredTicket, error) { return r.listed, nil }

func (r staleListingRepo) WithMutation(fn func(MutationRepository) error) error {
	return r.memRepo.WithMutation(func(MutationRepository) error { return fn(r) })
}

func TestEpicActivationRefreshesChildFiles(t *testing.T) {
	path := testutil.InitRepo(t)
	other := testutil.InitRepo(t)
	for _, tc := range []struct {
		name, oldParent, path, branch string
		wantErr                       bool
	}{
		{name: "child repository changed", oldParent: "epc-100", path: other, wantErr: true},
		{name: "child branch changed", oldParent: "epc-100", path: path, branch: "integration", wantErr: true},
		{name: "child joined epic", oldParent: "other", path: other, wantErr: true},
		{name: "child left epic", oldParent: "epc-100", path: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epc-100", fmt.Sprintf("---\nid: epc-100\nkind: epic\nstatus: open\npath: %q\nbranch: integration\n---\n# Epic\n", path))
			repo.add("child", fmt.Sprintf("---\nid: child\nparent: %s\npath: %q\n---\n# Child\n", tc.oldParent, path))
			listed, err := repo.List()
			require.NoError(t, err)
			parent := "epc-100"
			if tc.name == "child left epic" {
				parent = "other"
			}
			repo.add("child", fmt.Sprintf("---\nid: child\nparent: %s\npath: %q\nbranch: %q\n---\n# Child\n", parent, tc.path, tc.branch))
			runtime := &spyRuntime{}
			svc := New(Static(testCfg()), staleListingRepo{repo, listed}, runtime)
			_, err = svc.Init("epc-100", InitRequest{Status: "todo"})
			if tc.wantErr {
				require.ErrorIs(t, err, ErrInvalidState)
				assert.Empty(t, repo.saved)
				assert.Empty(t, runtime.enqueued)
				assert.True(t, repo.tickets["epc-100"].Ticket.IsPassiveEpic())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"epc-100"}, runtime.enqueued)
		})
	}
}

func TestAgentEpicOperations(t *testing.T) {
	path := testutil.InitRepo(t)
	for _, verb := range []string{"pause", "retry", "skip"} {
		t.Run(verb, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epc-100", "---\nid: epc-100\nkind: epic\nepic_execution: agent\nkontora: true\nstatus: human_review\n---\n# Epic\n")
			repo.add("child", "---\nid: child\nparent: epc-100\nstatus: in_progress\n---\n# Child\n")
			for _, st := range repo.tickets {
				require.NoError(t, st.Ticket.SetField("path", path))
			}
			runtime := &spyRuntime{}
			svc := New(Static(testCfg()), repo, runtime)
			var err error
			switch verb {
			case "pause":
				_, err = svc.SetStatus("epc-100", ticket.StatusPaused)
				assert.Equal(t, []string{"epc-100"}, runtime.cancelled)
			case "retry":
				_, err = svc.Retry("epc-100")
				assert.Equal(t, []string{"epc-100"}, runtime.enqueued)
			case "skip":
				_, err = svc.Skip("epc-100")
				require.ErrorIs(t, err, ErrInvalidState)
				assert.Empty(t, repo.saved)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{"epc-100"}, repo.saved)
			assert.Equal(t, ticket.StatusInProgress, repo.tickets["child"].Ticket.Status)
		})
	}
}

func TestAgentEpicSetParent(t *testing.T) {
	path := testutil.InitRepo(t)
	for _, childPath := range []string{path, testutil.InitRepo(t)} {
		repo := newMemRepo()
		repo.add("epc-100", fmt.Sprintf("---\nid: epc-100\nkind: epic\nepic_execution: agent\npath: %q\n---\n# Epic\n", path))
		repo.add("child", fmt.Sprintf("---\nid: child\npath: %q\npipeline: default\n---\n# Child\n", childPath))
		cfg := testCfg()
		cfg.Pipelines["child"] = cfg.Pipelines["default"]
		cfg.Projects = map[string]config.Project{"repo": {Path: path, EpicChildPipeline: "child"}}
		svc := New(Static(cfg), repo, &spyRuntime{})
		_, err := svc.SetParent("child", "epc-100")
		if childPath == path {
			require.NoError(t, err)
			assert.Equal(t, "epc-100", repo.tickets["child"].Ticket.Parent)
			assert.Equal(t, "default", repo.tickets["child"].Ticket.Pipeline)
		} else {
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Empty(t, repo.saved)
		}
		_, err = svc.SetParent("epc-100", "epc-100")
		require.Error(t, err)
	}
}

func TestAgentEpicRepositoryIdentity(t *testing.T) {
	path := testutil.InitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	out, err := exec.Command("git", "-C", path, "worktree", "add", "-b", "linked", linked).CombinedOutput()
	require.NoError(t, err, string(out))
	epic := &ticket.Ticket{ID: "e", Kind: ticket.KindEpic, EpicExecution: ticket.EpicExecutionAgent, Path: path}
	require.NoError(t, ValidateEpicChildRepository(epic, &ticket.Ticket{ID: "c", Path: linked}))
	require.ErrorIs(t, ValidateEpicChildRepository(epic, &ticket.Ticket{ID: "c", Path: testutil.InitRepo(t)}), ErrInvalidState)
	require.ErrorIs(t, ValidateEpicChildRepository(epic, &ticket.Ticket{ID: "c"}), ErrInvalidState)
}

func TestExecutionPreparationToleratesOnlyUnrelatedDiscoveryErrors(t *testing.T) {
	path := testutil.InitRepo(t)
	for _, related := range []bool{false, true} {
		t.Run(fmt.Sprintf("related=%v", related), func(t *testing.T) {
			repo := &parentReadRepo{memRepo: newMemRepo(), parentID: "broken", getErr: os.ErrPermission}
			repo.add("epic", fmt.Sprintf("---\nid: epic\nkind: epic\nepic_execution: agent\npath: %q\n---\n# Epic\n", path))
			parent := ""
			if related {
				parent = "epic"
			}
			repo.add("broken", fmt.Sprintf("---\nid: broken\nparent: %q\n---\n# Broken\n", parent))
			svc := New(Static(testCfg()), repo, NoopRuntime{})
			prepared, err := svc.PrepareExecutionSettings(repo.tickets["epic"].Ticket)
			if related {
				require.ErrorIs(t, err, os.ErrPermission)
				return
			}
			require.NoError(t, err)
			require.NoError(t, repo.WithMutation(func(scoped MutationRepository) error {
				return prepared.Validate(scoped, repo.tickets["epic"].Ticket)
			}))
		})
	}
}

func TestInitResolvesGitOnlyBeforeMutationScope(t *testing.T) {
	path := testutil.InitRepo(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	locked := filepath.Join(bin, "locked")
	script := fmt.Sprintf("#!/bin/sh\nif test -e %q; then exit 99; fi\nexec %q \"$@\"\n", locked, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	inner := newMemRepo()
	inner.add("epic", fmt.Sprintf("---\nid: epic\nkind: epic\nstatus: open\npath: %q\nbranch: integration\n---\n# Epic\n", path))
	inner.add("child", fmt.Sprintf("---\nid: child\nparent: epic\npath: %q\n---\n# Child\n", path))
	repo := transactionTestRepository{MutationRepository: inner, withMutation: func(fn func(MutationRepository) error) error {
		return inner.WithMutation(func(scoped MutationRepository) error {
			require.NoError(t, os.WriteFile(locked, nil, 0o600))
			defer os.Remove(locked)
			return fn(scoped)
		})
	}}
	_, err = New(Static(testCfg()), repo, NoopRuntime{}).Init("epic", InitRequest{Status: "todo"})
	require.NoError(t, err)
}

const epicFM = "---\nid: epc-100\nstatus: open\nkontora: true\nkind: epic\n---\n# An epic\n"

// Passive epics derive status from children. The invalid operations below are
// refused at the layer shared by the CLI, TUI and daemon.
func TestEpicRefusedByServiceVerbs(t *testing.T) {
	cases := []struct {
		name string
		call func(*Service) error
	}{
		{name: "set status", call: func(s *Service) error { _, err := s.SetStatus("epc-100", ticket.StatusDone); return err }},
		{name: "retry", call: func(s *Service) error { _, err := s.Retry("epc-100"); return err }},
		{name: "skip", call: func(s *Service) error { _, err := s.Skip("epc-100"); return err }},
		{name: "init", call: func(s *Service) error { _, err := s.Init("epc-100", InitRequest{Pipeline: "default"}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			repo.add("epc-100", epicFM)
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			err := tc.call(svc)
			require.ErrorIs(t, err, ErrInvalidState)
			assert.Empty(t, repo.saved)
			assert.Equal(t, ticket.StatusOpen, repo.tickets["epc-100"].Ticket.Status)
		})
	}
}

func TestSetParent(t *testing.T) {
	cases := []struct {
		name       string
		tickets    map[string]string
		child      string
		parent     string
		wantErr    error
		wantParent string
	}{
		{
			name: "a ticket is filed under an epic",
			tickets: map[string]string{
				"epc-100": epicFM,
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\n---\n# A task\n",
			},
			child: "tsk-001", parent: "epc-100", wantParent: "epc-100",
		},
		{
			name: "a deleted former parent does not prevent moving",
			tickets: map[string]string{
				"epc-100": epicFM,
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\nparent: epc-deleted\n---\n# A task\n",
			},
			child: "tsk-001", parent: "epc-100", wantParent: "epc-100",
		},
		{
			name: "a managed former parent still prevents moving",
			tickets: map[string]string{
				"epc-100": epicFM,
				"epc-old": "---\nid: epc-old\nkind: epic\nmanaged: {}\n---\n# Managed epic\n",
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\nparent: epc-old\n---\n# A task\n",
			},
			child: "tsk-001", parent: "epc-100", wantErr: ticket.ErrManaged,
		},
		{
			name: "the parent must be an epic",
			tickets: map[string]string{
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\n---\n# A task\n",
				"tsk-002": "---\nid: tsk-002\nstatus: open\nkontora: true\n---\n# Another\n",
			},
			child: "tsk-001", parent: "tsk-002", wantErr: ErrNotAnEpic,
		},
		{
			name: "epics do not nest",
			tickets: map[string]string{
				"epc-100": epicFM,
				"epc-200": "---\nid: epc-200\nstatus: open\nkontora: true\nkind: epic\n---\n# Another epic\n",
			},
			child: "epc-200", parent: "epc-100", wantErr: ErrNotAnEpic,
		},
		{
			name:    "a ticket cannot parent itself",
			tickets: map[string]string{"epc-100": epicFM},
			child:   "epc-100", parent: "epc-100", wantErr: ErrSelfRelation,
		},
		{
			name: "a parent chain that would close a cycle",
			tickets: map[string]string{
				// A hand-written file can put an epic under a task, which is the
				// only way a two-step parent cycle can be reached.
				"epc-100": "---\nid: epc-100\nstatus: open\nkontora: true\nkind: epic\nparent: tsk-001\n---\n# An epic\n",
				"tsk-001": "---\nid: tsk-001\nstatus: open\nkontora: true\n---\n# A task\n",
			},
			child: "tsk-001", parent: "epc-100", wantErr: ErrRelationCycle,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			for id, fm := range tc.tickets {
				repo.add(id, fm)
			}
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			_, err := svc.SetParent(tc.child, tc.parent)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, repo.saved)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []string{tc.child}, repo.saved)
			assert.Equal(t, tc.wantParent, repo.tickets[tc.child].Ticket.Parent)
		})
	}
}

// Clearing a parent writes the child. Clearing one that is not there writes
// nothing, so a repeated call is not a repeated file write.
func TestClearParent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		parent  string
		wantErr error
	}{
		{name: "existing", parent: epicFM},
		{name: "deleted"},
		{name: "managed", parent: "---\nid: epc-100\nkind: epic\nmanaged: {}\n---\n# Managed epic\n", wantErr: ticket.ErrManaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newMemRepo()
			if tc.parent != "" {
				repo.add("epc-100", tc.parent)
			}
			repo.add("tsk-001", "---\nid: tsk-001\nstatus: open\nkontora: true\nparent: epc-100\n---\n# A task\n")
			svc := New(Static(testCfg()), repo, &spyRuntime{})

			_, err := svc.ClearParent("tsk-001")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Empty(t, repo.saved)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, repo.tickets["tsk-001"].Ticket.Parent)
			assert.Equal(t, []string{"tsk-001"}, repo.saved)

			_, err = svc.ClearParent("tsk-001")
			require.NoError(t, err)
			assert.Equal(t, []string{"tsk-001"}, repo.saved)
		})
	}
}

// The order list writes one file, the epic's, and never a child.
func TestSetChildOrder(t *testing.T) {
	newRepo := func() *memRepo {
		repo := newMemRepo()
		repo.add("epc-100", epicFM)
		repo.add("tsk-001", "---\nid: tsk-001\nstatus: open\nkontora: true\nparent: epc-100\n---\n# One\n")
		repo.add("tsk-002", "---\nid: tsk-002\nstatus: open\nkontora: true\nparent: epc-100\n---\n# Two\n")
		return repo
	}

	t.Run("writes the epic only", func(t *testing.T) {
		repo := newRepo()
		svc := New(Static(testCfg()), repo, &spyRuntime{})
		_, err := svc.SetChildOrder("epc-100", []string{"tsk-002", "tsk-001"})
		require.NoError(t, err)
		assert.Equal(t, []string{"epc-100"}, repo.saved)
		assert.Equal(t, []string{"tsk-002", "tsk-001"}, repo.tickets["epc-100"].Ticket.Children)
	})

	t.Run("the same order writes nothing", func(t *testing.T) {
		repo := newRepo()
		svc := New(Static(testCfg()), repo, &spyRuntime{})
		_, err := svc.SetChildOrder("epc-100", []string{"tsk-002", "tsk-001"})
		require.NoError(t, err)
		_, err = svc.SetChildOrder("epc-100", []string{"tsk-002", "tsk-001"})
		require.NoError(t, err)
		assert.Equal(t, []string{"epc-100"}, repo.saved)
	})

	t.Run("only an epic has an order", func(t *testing.T) {
		repo := newRepo()
		svc := New(Static(testCfg()), repo, &spyRuntime{})
		_, err := svc.SetChildOrder("tsk-001", []string{"tsk-002"})
		require.ErrorIs(t, err, ErrNotAnEpic)
		assert.Empty(t, repo.saved)
	})
}

// Passive epics have no execution; agent epics use the standalone session key.
func TestBuildViewEpicHasNoPipelineData(t *testing.T) {
	tkt, err := ticket.ParseBytes([]byte(epicFM))
	require.NoError(t, err)

	v := BuildView(testCfg(), tkt, true)
	assert.Equal(t, "epic", v.Kind)
	assert.Empty(t, v.Agent)
	assert.Empty(t, v.Stages)

	require.NoError(t, tkt.SetField("epic_execution", ticket.EpicExecutionAgent))
	v = BuildView(testCfg(), tkt, true)
	assert.Equal(t, ticket.EpicExecutionAgent, v.EpicExecution)
	assert.Equal(t, testCfg().DefaultAgent, v.Agent)
	assert.Equal(t, []string{"default"}, v.Stages)
	assert.Equal(t, "default", v.Stage)
	require.NoError(t, tkt.SetField("stage", "rework"))
	v = BuildView(testCfg(), tkt, true)
	assert.Equal(t, "default", v.Stage, "feedback runs retain the supervisor session key")
}
