package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/hook"
	"github.com/worksonmyai/kontora/internal/process"
)

// Only values used by this launch belong here. Other agents and service credentials must not enter run artifacts.
type automationLaunch struct {
	Version      int                    `json:"version"`
	AutomationID string                 `json:"automation_id"`
	Number       uint64                 `json:"number"`
	InvocationID string                 `json:"invocation_id"`
	SpecHash     string                 `json:"spec_hash"`
	Agent        automationLaunchAgent  `json:"agent"`
	Executable   string                 `json:"executable"`
	Env          map[string]string      `json:"env"`
	Model        string                 `json:"model"`
	Effort       string                 `json:"effort"`
	Project      string                 `json:"project"`
	RepoPath     string                 `json:"repo_path"`
	Checkout     string                 `json:"checkout"`
	Worktree     string                 `json:"worktree"`
	Timeout      time.Duration          `json:"timeout"`
	Hooks        map[string][]hook.Spec `json:"hooks"`
	Paths        automationLaunchPaths  `json:"paths"`
}

type automationLaunchAgent struct {
	Binary                     string   `json:"binary"`
	Args                       []string `json:"args"`
	FailurePatterns            []string `json:"failure_patterns"`
	Effort                     string   `json:"effort"`
	CheckpointCompactionTokens int      `json:"checkpoint_compaction_tokens"`
}

func (a automationLaunchAgent) config() config.Agent {
	return config.Agent{Binary: a.Binary, Args: slices.Clone(a.Args), FailurePatterns: slices.Clone(a.FailurePatterns), Effort: a.Effort, CheckpointCompactionTokens: a.CheckpointCompactionTokens}
}

type automationLaunchPaths struct {
	Dir      string `json:"dir"`
	Result   string `json:"result"`
	Composed string `json:"composed"`
	Capture  string `json:"capture"`
	Agent    string `json:"agent"`
	Events   string `json:"events"`
	Hooks    string `json:"hooks"`
	Terminal string `json:"terminal"`
}

func automationPaths(dir string) automationLaunchPaths {
	return automationLaunchPaths{Dir: dir, Result: filepath.Join(dir, "result.json"), Composed: filepath.Join(filepath.Dir(dir), filepath.Base(dir)+".log"), Capture: filepath.Join(dir, "capture.log"), Agent: filepath.Join(dir, "agent.log"), Events: filepath.Join(dir, "events.json"), Hooks: filepath.Join(dir, "hooks.log"), Terminal: filepath.Join(dir, "terminal.json")}
}

func automationSpecHash(spec automation.Spec) string {
	data, _ := json.Marshal(spec)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (d *Daemon) freezeAutomationRun(run automation.Run, cfg *config.Config) (automation.Run, error) {
	fail := func(cause error) (automation.Run, error) {
		d.failAutomationRun(run, cause, false)
		if saved, err := d.automationStore.GetRun(run.AutomationID, run.N); err == nil {
			run = saved
		}
		return run, cause
	}
	if run.Phase != "queued" || run.ResultPath != "" {
		return run, automation.ErrConflict
	}
	root, err := filepath.Abs(config.ExpandTilde(cfg.LogsDir))
	if err != nil {
		return fail(err)
	}
	if err := makeAutomationDirectory(root); err != nil {
		return fail(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fail(err)
	}
	parent := filepath.Join(root, "automations", run.Key)
	for _, path := range []string{filepath.Dir(parent), parent} {
		if err := privateAutomationDirectory(path); err != nil {
			return fail(err)
		}
	}
	dir := filepath.Join(parent, fmt.Sprint(run.N))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fail(fmt.Errorf("reserving run artifacts: %w", err))
	}
	if err := syncAutomationDirectory(parent); err != nil {
		return fail(err)
	}
	paths := automationPaths(dir)
	updated, err := d.automationStore.UpdateRun(run.AutomationID, run.N, func(r *automation.Run) error {
		if r.Phase != "queued" || r.ResultPath != "" {
			return automation.ErrConflict
		}
		r.LogPath, r.ResultPath = paths.Composed, paths.Result
		return nil
	})
	if err != nil {
		return fail(err)
	}
	run = updated
	for _, path := range []string{paths.Capture, paths.Agent, paths.Hooks, paths.Composed} {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return fail(err)
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return fail(err)
		}
	}
	launch, err := d.resolveAutomationLaunch(run, cfg, paths)
	if err != nil {
		return fail(err)
	}
	data, err := json.Marshal(launch)
	if err != nil {
		return fail(err)
	}
	if err := durableConfigWrite(filepath.Join(dir, "launch.json"), data, 0o600); err != nil {
		return fail(err)
	}
	return run, nil
}

func (d *Daemon) resolveAutomationLaunch(run automation.Run, cfg *config.Config, paths automationLaunchPaths) (automationLaunch, error) {
	agent, ok := cfg.Agents[run.Spec.Agent]
	if !ok {
		return automationLaunch{}, fmt.Errorf("agent %q is not configured", run.Spec.Agent)
	}
	if run.Spec.Model != "" && agent.Kind() == "" {
		return automationLaunch{}, fmt.Errorf("agent %q does not support a model override", run.Spec.Agent)
	}
	model, effort := agent.Effective(run.Spec.Model, "")
	if err := agent.CheckEffort(model, effort); err != nil {
		return automationLaunch{}, err
	}
	binary, err := d.agentLookup(agent.Binary)
	if err == nil && binary == "" {
		err = errors.New("agent lookup returned no executable")
	}
	if err != nil {
		return automationLaunch{}, err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return automationLaunch{}, err
	}
	repo, err := canonicalAutomationPath(config.ExpandTilde(run.Spec.Path))
	if err != nil {
		return automationLaunch{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	checkout, err := automationGit(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return automationLaunch{}, err
	}
	checkout, err = canonicalAutomationPath(checkout)
	if err != nil {
		return automationLaunch{}, err
	}
	timeout, err := time.ParseDuration(run.Spec.Timeout)
	if err != nil || timeout <= 0 {
		return automationLaunch{}, fmt.Errorf("invalid automation timeout %q", run.Spec.Timeout)
	}
	project, _, _ := cfg.ProjectFor(run.Spec.Path)
	launch := automationLaunch{Version: 1, AutomationID: run.AutomationID, Number: run.N, InvocationID: run.InvocationID, SpecHash: automationSpecHash(run.Spec),
		Executable: binary, Agent: automationLaunchAgent{Binary: agent.Binary, Args: slices.Clone(agent.Args), FailurePatterns: slices.Clone(agent.FailurePatterns), Effort: agent.Effort, CheckpointCompactionTokens: agent.CheckpointCompactionTokens},
		Env: automationLaunchEnv(cfg, agent, d.configPath, run), Model: model, Effort: effort, Project: project, RepoPath: repo, Checkout: checkout, Worktree: repo, Timeout: timeout, Paths: paths, Hooks: map[string][]hook.Spec{}}
	if run.Spec.Workspace == "worktree" {
		launch.Worktree, err = filepath.Abs(d.worktrees.Path(filepath.Base(repo), fmt.Sprintf("%s-%d", run.AutomationID, run.N)))
		if err != nil {
			return automationLaunch{}, err
		}
	}
	for _, event := range []string{config.HookWorktreeCreated, config.HookStageStart, config.HookStageEnd} {
		launch.Hooks[event] = []hook.Spec{}
		for _, spec := range cfg.HooksFor(run.Spec.Path, event) {
			launch.Hooks[event] = append(launch.Hooks[event], hook.Spec{Name: spec.Name, Run: spec.Run, Timeout: spec.TimeoutOrDefault(), Fatal: spec.Fatal(event)})
		}
	}
	return launch, nil
}

func automationReservedEnv(key string) bool {
	return strings.HasPrefix(key, "KONTORA_RESULT_") || strings.HasPrefix(key, "KONTORA_OPERATOR_") || strings.HasPrefix(key, "KONTORA_MANAGED_") || strings.HasPrefix(key, "KONTORA_ASSISTANT_") || key == "KONTORA_TICKET_ID" || key == "KONTORA_TICKET_FILE" || key == "KONTORA_STAGE" || key == "KONTORA_INVOCATION_ID" || key == "KONTORA_CHECKPOINT_FILE" || key == "KONTORA_AUTOMATION_ID" || key == "KONTORA_RUN_NUMBER"
}

func automationLaunchEnv(cfg *config.Config, agent config.Agent, configPath string, run automation.Run) map[string]string {
	env := agentEnv(cfg, agent, configPath, run.Spec.Agent, "", "")
	// Empty agent values must also clear inherited variables, not merely remove a global override.
	for key, value := range agent.Environment {
		if value == "" {
			env[key] = ""
		}
	}
	for key := range env {
		if automationReservedEnv(key) {
			env[key] = ""
		}
	}
	env["KONTORA_AUTOMATION_ID"], env["KONTORA_RUN_NUMBER"] = run.AutomationID, fmt.Sprint(run.N)
	return env
}

func automationExecutionEnv(launch automationLaunch) map[string]string {
	env := maps.Clone(launch.Env)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if automationReservedEnv(key) {
			env[key] = ""
		}
	}
	env["KONTORA_AUTOMATION_ID"], env["KONTORA_RUN_NUMBER"] = launch.AutomationID, fmt.Sprint(launch.Number)
	return env
}

func canonicalAutomationPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
func makeAutomationDirectory(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", path)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(path)
	if err := makeAutomationDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return syncAutomationDirectory(parent)
}

func privateAutomationDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("automation directory is not a directory: %s", path)
		}
		return os.Chmod(path, 0o700)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return err
	}
	return syncAutomationDirectory(filepath.Dir(path))
}
func syncAutomationDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func readAutomationLaunch(run automation.Run) (automationLaunch, error) {
	var launch automationLaunch
	if !filepath.IsAbs(run.ResultPath) || filepath.Base(run.ResultPath) != "result.json" || filepath.Clean(run.ResultPath) != run.ResultPath {
		return launch, errors.New("run has no frozen launch paths")
	}
	path := filepath.Join(filepath.Dir(run.ResultPath), "launch.json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return launch, fmt.Errorf("reading frozen launch: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return launch, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return launch, errors.New("frozen launch must be a private regular file under 1 MiB")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return launch, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&launch); err != nil {
		return launch, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return launch, errors.New("trailing frozen launch data")
	}
	if launch.Version != 1 || launch.AutomationID != run.AutomationID || launch.Number != run.N || launch.InvocationID != run.InvocationID || launch.SpecHash != automationSpecHash(run.Spec) {
		return launch, errors.New("frozen launch identity does not match run")
	}
	paths := automationPaths(filepath.Dir(run.ResultPath))
	if launch.Paths != paths || run.LogPath != paths.Composed || launch.Env == nil || launch.Hooks == nil || launch.Agent.Binary == "" || !filepath.IsAbs(launch.Executable) || !filepath.IsAbs(launch.RepoPath) || !filepath.IsAbs(launch.Checkout) || !filepath.IsAbs(launch.Worktree) {
		return launch, errors.New("invalid frozen launch paths or inputs")
	}
	timeout, err := time.ParseDuration(run.Spec.Timeout)
	if err != nil || timeout <= 0 || timeout != launch.Timeout {
		return launch, errors.New("invalid frozen launch timeout")
	}
	return launch, nil
}

type automationRepoKey struct {
	daemon   *Daemon
	checkout string
}
type automationRepoLease struct {
	invocation string
	done       chan struct{}
}

var automationRepoLeases = struct {
	sync.Mutex
	holders map[automationRepoKey]automationRepoLease
}{holders: map[automationRepoKey]automationRepoLease{}}

func (d *Daemon) acquireAutomationRepoLock(ctx context.Context, run automation.Run, launch automationLaunch) error {
	if run.Spec.Workspace != "repo" {
		return nil
	}
	key := automationRepoKey{d, launch.Checkout}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		automationRepoLeases.Lock()
		held, ok := automationRepoLeases.holders[key]
		if !ok {
			automationRepoLeases.holders[key] = automationRepoLease{run.InvocationID, make(chan struct{})}
			automationRepoLeases.Unlock()
			return nil
		}
		automationRepoLeases.Unlock()
		if held.invocation == run.InvocationID {
			return errors.Join(automation.ErrConflict, process.ErrTerminationUncertain)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-held.done:
		}
	}
}

// Recovery reserves the checkout before admitting another run, including runs of a different automation.
func (d *Daemon) retainAutomationRepoLock(run automation.Run) error {
	if run.Spec.Workspace != "repo" {
		return nil
	}
	launch, err := readAutomationLaunch(run)
	if err != nil {
		return err
	}
	key := automationRepoKey{d, launch.Checkout}
	automationRepoLeases.Lock()
	defer automationRepoLeases.Unlock()
	if held, ok := automationRepoLeases.holders[key]; ok {
		if held.invocation == run.InvocationID {
			return nil
		}
		return fmt.Errorf("%w: checkout has another unfinished invocation", automation.ErrConflict)
	}
	automationRepoLeases.holders[key] = automationRepoLease{run.InvocationID, make(chan struct{})}
	return nil
}

func (d *Daemon) releaseAutomationRepoLock(run automation.Run) {
	automationRepoLeases.Lock()
	defer automationRepoLeases.Unlock()
	for key, held := range automationRepoLeases.holders {
		if key.daemon == d && held.invocation == run.InvocationID {
			delete(automationRepoLeases.holders, key)
			close(held.done)
		}
	}
}
