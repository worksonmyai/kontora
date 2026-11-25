package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/hook"
	"github.com/worksonmyai/kontora/internal/pipeline"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/prompt"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/ticket"
)

var errWorkflowNoSpawn = errors.New("workflow admission did not select an executable node")

type workflowExecutionKey struct{}

func workflowExecutionFrom(ctx context.Context) *executionRecord {
	rec, _ := ctx.Value(workflowExecutionKey{}).(*executionRecord)
	return rec
}

func workflowFingerprint(cfg *config.Config, t *ticket.Ticket, graph *config.WorkflowGraph) (string, error) {
	normalized := *graph
	normalized.Nodes = slices.Clone(graph.Nodes)
	normalized.Edges = slices.Clone(graph.Edges)
	slices.SortFunc(normalized.Nodes, func(a, b config.WorkflowNode) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(normalized.Edges, func(a, b config.WorkflowEdge) int {
		return strings.Compare(a.From.Node+"\x00"+a.From.Port+"\x00"+a.To, b.From.Node+"\x00"+b.From.Port+"\x00"+b.To)
	})
	stages := make(map[string]config.Stage)
	agents := make(map[string]config.Agent)
	for i := range normalized.Nodes {
		normalized.Nodes[i].Verdicts = slices.Clone(normalized.Nodes[i].Verdicts)
		slices.Sort(normalized.Nodes[i].Verdicts)
	}
	for _, node := range normalized.Nodes {
		if node.Type != "agent" {
			continue
		}
		stages[node.Stage] = cfg.Stages[node.Stage]
		name := node.Agent
		if t.Agent != "" {
			name = t.Agent
		}
		agent, ok := cfg.Agents[name]
		if !ok {
			return "", fmt.Errorf("unknown workflow agent %q", name)
		}
		agents[name] = agent
	}
	hooks := make(map[string][]config.Hook)
	for _, event := range []string{config.HookWorktreeCreated, config.HookStageStart, config.HookStageEnd} {
		hooks[event] = cfg.HooksFor(t.Path, event)
	}
	data, err := json.Marshal(struct {
		Graph        config.WorkflowGraph
		Stages       map[string]config.Stage
		Agents       map[string]config.Agent
		Override     string
		Hooks        map[string][]config.Hook
		SystemPrompt string
		ResumePrompt string
		Environment  map[string]string
	}{normalized, stages, agents, t.Agent, hooks, cfg.SystemPrompt, cfg.ResumePrompt, cfg.Environment})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (d *Daemon) initializeWorkflow(cfg *config.Config, t *ticket.Ticket, graph *config.WorkflowGraph) error {
	pause := func(reason string) error {
		_ = t.SetField("status", string(ticket.StatusPaused))
		_ = t.SetField("last_error", reason)
		return errWorkflowNoSpawn
	}
	if t.AnnotationReturnStatus != "" || t.IsAgentEpic() {
		return pause("graph workflows do not support annotation runs or epic supervisors")
	}
	fingerprint, err := workflowFingerprint(cfg, t, graph)
	if err != nil {
		return pause(err.Error())
	}
	if rec, loadErr := loadExecution(cfg, t.ID); !errors.Is(loadErr, os.ErrNotExist) {
		if loadErr != nil {
			return pause("workflow invocation journal is unreadable; inspect before recovery: " + loadErr.Error())
		}
		if rec.GraphBefore != nil {
			return pause("workflow invocation journal requires recovery before another invocation")
		}
	}
	state := t.GraphState
	stage := t.Stage
	if state == nil {
		if stage != "" && stage != graph.Entry {
			return pause("a new graph workflow must start at its entry")
		}
		state = &ticket.GraphState{Version: workflowStateVersion(graph), ExecutionID: newSessionID(), DefinitionFingerprint: fingerprint}
		stage = ""
	} else {
		if state.DefinitionFingerprint != fingerprint {
			return pause("workflow definition changed; inspect the saved execution before restarting")
		}
		if t.InvocationID != "" {
			return pause("workflow execution requires recovery acknowledgement before another invocation")
		}
		if node, ok := graph.Node(stage); ok && node.Type == "end" {
			return pause("workflow is at an end node; use workflow-restart")
		}
	}
	state = state.Clone()
	if reservation := workflowReservation(t); reservation != nil {
		if err := d.validateWaitProgress(t, reservation); err != nil {
			return pause(err.Error())
		}
	}
	var ready *ticket.WorkflowWait
	if state.Wait != nil {
		if state.Wait.Phase != "ready" || state.Wait.Disarmed || !state.Continuation {
			return pause("workflow wait must be released before continuation")
		}
		ready, state.Wait = state.Wait, nil
	}
	populateWorkflowDisplay(state, graph)
	after := pipeline.AdvanceGraph(graph, stage, state, nil)
	if ready != nil {
		after.State.Wait = ready
	}
	if err := d.finishControlAdvance(t, after); err != nil {
		return err
	}
	if after.Next == nil {
		if after.Status == ticket.StatusDone {
			_ = t.SetField("completed_at", time.Now())
		}
		return errWorkflowNoSpawn
	}
	return nil
}

// Call only for a new execution or after verifying its definition fingerprint.
func populateWorkflowDisplay(state *ticket.GraphState, graph *config.WorkflowGraph) {
	if state.NodeTemplates == nil {
		state.NodeTemplates = make(map[string]string)
		for _, node := range graph.Nodes {
			if node.Type == "agent" {
				state.NodeTemplates[node.ID] = node.Stage
			}
		}
	}
	if state.LoopLimits == nil {
		state.LoopLimits = make(map[string]int)
		for _, node := range graph.Nodes {
			if node.Type == "loop" {
				state.LoopLimits[node.ID] = node.Max
			}
		}
	}
}

func installGraphAdvance(t *ticket.Ticket, after pipeline.GraphAdvance) error {
	if err := t.SetField("graph_state", after.State); err != nil {
		return err
	}
	if err := t.SetField("stage", after.Stage); err != nil {
		return err
	}
	status := after.Status
	if after.Next != nil {
		status = ticket.StatusTodo
	}
	if err := t.SetField("status", string(status)); err != nil {
		return err
	}
	return t.SetField("last_error", after.Reason)
}

func (d *Daemon) bindAgentExecution(t *ticket.Ticket, p spawnAgentParams, params *RunnerParams, epicID string) (*executionRecord, error) {
	rec := p.boundExecution
	if rec == nil {
		return d.bindExecution(t, p, params, epicID)
	}
	maps.Copy(params.Env, rec.Invocation.Env())
	params.RequireTermination = true
	params.TerminalReported = func() bool { return terminalReported(rec.Invocation.ResultPath) }
	params.LogFile = graphLogPath(p.cfg, t)
	rec.Phase = "running"
	return rec, saveExecution(p.cfg, rec)
}

func workflowTerminationUncertain(p spawnAgentParams, result process.Result, err error) bool {
	if errors.Is(err, process.ErrTerminationUncertain) {
		return true
	}
	return p.graphState != nil && result.Termination == process.TerminationUnknown
}

func recordRunnerResult(rec *executionRecord, result *process.Result, err error) {
	if rec != nil {
		rec.RunnerTermination = result.Termination
		if rec.GraphBefore != nil {
			preserveWorkflowInterruption(result, err)
		}
	}
}

func classifyGraphLaunch(p spawnAgentParams, run *agentRun) {
	if p.graphState != nil && run.Result.Termination == process.TerminationNotStarted {
		run.Outcome, run.OutcomeReason, run.ForcePause = "failed", "workflow agent did not start", true
	}
}

func graphLogPath(cfg *config.Config, t *ticket.Ticket) string {
	return session.GraphLogPath(expandTilde(cfg.LogsDir), t.ID, t.Stage, t.GraphState.VisitID, t.InvocationID)
}

func (d *Daemon) runGraphTicket(root, task context.Context, cfg *config.Config, log *slog.Logger, t *ticket.Ticket, path string) {
	definition := cfg.Pipelines[t.Pipeline]
	graph := definition.Graph
	node, ok := graph.Node(t.Stage)
	if !ok || (node.Type != "agent" && node.Type != "check") || t.GraphState == nil {
		d.pauseExecution(task, t, path, "workflow admission selected an invalid executable node")
		return
	}
	resolved, err := definition.Resolve(t.Stage, t.Agent)
	if err != nil {
		d.pauseExecution(task, t, path, err.Error())
		return
	}
	p := spawnAgentParams{cfg: cfg, ctx: root, log: log, ticketID: t.ID, filePath: path,
		agentName: resolved.Agent, pipelineName: t.Pipeline, stageName: node.ID, run: stageRunIndex(t, node.ID),
		branch: t.Branch, graphState: t.GraphState.Clone(), isPipeline: true}
	if node.Type == "agent" {
		p.agentCfg = cfg.Agents[resolved.Agent]
		p.stageCfg = cfg.Stages[resolved.Template]
	}
	epicID, err := d.supervision(t, false)
	if err != nil {
		d.pauseExecution(task, t, path, err.Error())
		return
	}
	p.supervisionID, p.supervisionResolved = epicID, true
	params := RunnerParams{Env: make(map[string]string)}
	rec, err := d.bindExecution(t, p, &params, epicID)
	if err != nil {
		d.pauseExecution(task, t, path, "persist workflow invocation: "+err.Error())
		return
	}
	p.boundExecution = rec
	task = context.WithValue(task, workflowExecutionKey{}, rec)
	run := agentRun{Execution: rec, Outcome: "failed", ForcePause: true,
		Result: process.Result{ExitCode: -1, StartedAt: time.Now(), Termination: process.TerminationNotStarted}}
	stageStartCompleted := false
	finishFailure := func(reason string) {
		run.OutcomeReason = reason
		run.Result.ExitedAt = time.Now()
		d.finishGraphTicket(root, task, cfg, log, t, path, p, node, run, stageStartCompleted)
	}
	repoPath, wtPath, prepared := d.prepareWorktreeForAgent(task, prepareWorktreeParams{
		cfg: cfg, log: log, t: t, filePath: path, ticketID: t.ID, stageName: node.ID,
		agentName: resolved.Agent, branch: t.Branch, isPipeline: true})
	if !prepared {
		// A failed preparation may already have applied an operator-visible pause.
		if ordinaryReservationRetained(task) {
			d.pauseUncertainWorkflow(task, cfg, t, path, rec)
			return
		}
		finishFailure("workflow preparation interrupted; inspect before recovery")
		return
	}
	p.repoPath, p.wtPath = repoPath, wtPath
	rec.Worktree = wtPath
	if err := saveExecution(cfg, rec); err != nil {
		finishFailure("persist workflow worktree: " + err.Error())
		return
	}
	hc := hook.Context{Event: config.HookStageStart, TicketID: t.ID, TicketFile: path,
		Worktree: wtPath, RepoPath: repoPath, Branch: t.Branch, Stage: node.ID, Agent: resolved.Agent}
	if err := d.runHooks(task, cfg, log, hc); err != nil {
		if ordinaryReservationRetained(task) {
			d.pauseUncertainWorkflow(task, cfg, t, path, rec)
			return
		}
		finishFailure("workflow stage_start: " + err.Error())
		return
	}
	stageStartCompleted = true
	if !d.executionCurrent(task, t, path) {
		finishFailure("workflow interrupted before launch")
		return
	}
	if node.Type == "check" {
		run = d.runCheckNode(task, cfg, t, node, rec, wtPath)
	} else {
		binary, lookupErr := d.agentLookup(p.agentCfg.Binary)
		if lookupErr != nil {
			finishFailure("workflow agent unavailable: " + lookupErr.Error())
			return
		}
		rendered, renderErr := d.renderTicketPrompt(cfg, p.stageCfg.Prompt, t, path, wtPath)
		if renderErr != nil {
			finishFailure("render workflow prompt: " + renderErr.Error())
			return
		}
		if h := rec.GraphBefore.Handoff; node.Handoff == config.WorkflowHandoffPrevious && h != nil &&
			h.ExecutionID == rec.Invocation.WorkflowExecutionID && h.DestinationVisitID == rec.Invocation.NodeVisitID {
			rendered += prompt.PreviousHandoff(h)
		}
		p.rendered = rendered + buildOperationalAppendix(t.ID, path, wtPath, true, checkpointKind(p.agentCfg)) + graphResultInstructions(node)
		attempt := d.runAgentOnce(task, t, p, binary, nil)
		if !attempt.started && attempt.run.Result.StartedAt.IsZero() {
			attempt.run.Result = run.Result
			attempt.run.Result.ExitedAt = time.Now()
		}
		run = attempt.run
		if run.Execution == nil {
			run.Execution = rec
		}
		if attempt.pauseReason != "" {
			run.ForcePause, run.Outcome, run.OutcomeReason = true, "failed", attempt.pauseReason
		}
		if !attempt.ok && run.OutcomeReason == "" {
			run.ForcePause, run.Outcome, run.OutcomeReason = true, "failed", "workflow invocation did not complete"
		}
	}
	if ordinaryReservationRetained(task) {
		d.pauseUncertainWorkflow(task, cfg, t, path, rec)
		return
	}
	d.finishGraphTicket(root, task, cfg, log, t, path, p, node, run, stageStartCompleted)
}

func graphResultInstructions(node config.WorkflowNode) string {
	version := 2
	if node.Wake != nil {
		version = 3
	}
	text := fmt.Sprintf("\n\n## Workflow result\nPublish an invocation-bound version %d result with kontora_result or kontora result before ending. Use complete for finished node work, or blocked with next_step when work cannot continue. Workflow participation grants no Git or publication permissions.\n", version)
	if node.Wake != nil {
		text += "Use kontora wait or kontora_wait to suspend until a timer or condition is ready. Include continuation instructions. Publication is not confirmation that suspension committed.\n"
	}
	if len(node.Verdicts) != 0 {
		verdicts, _ := json.Marshal(node.Verdicts)
		text += fmt.Sprintf("A complete result must include one declared verdict: %s. A request for changes is complete with the matching verdict.\n", verdicts)
	}
	return text
}

func (d *Daemon) pauseUncertainWorkflow(ctx context.Context, cfg *config.Config, t *ticket.Ticket, path string, rec *executionRecord) {
	rec.Phase = "recovery_required"
	if err := saveExecution(cfg, rec); err != nil {
		d.ticketLog(t.ID).Error("persist uncertain workflow", "err", err)
	}
	d.pauseExecution(context.WithoutCancel(ctx), t, path, "workflow termination is unconfirmed; inspect processes before recovery acknowledgement")
}

func (d *Daemon) pauseInterruptedWorkflow(ctx context.Context, cfg *config.Config, t *ticket.Ticket, path string, p spawnAgentParams, run agentRun) {
	rec := run.Execution
	run.ForcePause, run.Outcome, run.OutcomeReason = true, "failed", "workflow interrupted; inspect execution before recovery acknowledgement"
	rec.Phase = "recovery_required"
	if err := saveExecution(cfg, rec); err != nil {
		d.retainFinalizationReservation(ctx, t)
		return
	}
	_, err := d.patchExecution(context.WithoutCancel(ctx), t, path, func(current *ticket.Ticket) error {
		appendRunHistory(current, run, p.agentName, p.stageName, p.run)
		_ = current.SetField("status", string(ticket.StatusPaused))
		return current.SetField("last_error", run.OutcomeReason)
	})
	if err != nil && !errors.Is(err, errExecutionChanged) {
		d.retainFinalizationReservation(ctx, t)
	}
}

func (d *Daemon) finishGraphTicket(root, task context.Context, cfg *config.Config, log *slog.Logger, t *ticket.Ticket, path string, p spawnAgentParams, node config.WorkflowNode, run agentRun, stageStartCompleted bool) {
	rec := run.Execution
	if rec == nil {
		return
	}
	rec.RunnerTermination = run.Result.Termination
	if run.Result.Interruption != nil || task.Err() != nil {
		d.pauseInterruptedWorkflow(task, cfg, t, path, p, run)
		return
	}
	if err := d.beginFinalization(cfg, rec, run); err != nil {
		d.retainFinalizationReservation(task, t)
		log.Error("persist workflow finalization", "err", err)
		return
	}
	capture := freezeWorkflowCheckLog(cfg, log, t, node, run)
	var hookWarned bool
	if stageStartCompleted && task.Err() == nil && d.executionCurrent(task, t, path) {
		warned, hookErr := d.runHooksWithWarnings(task, cfg, log, hook.Context{Event: config.HookStageEnd,
			TicketID: t.ID, TicketFile: path, Worktree: p.wtPath, RepoPath: p.repoPath, Branch: t.Branch,
			Stage: node.ID, Agent: p.agentName, ExitCode: new(run.Result.ExitCode), RunOutcome: run.Outcome})
		hookWarned = warned
		if hookErr != nil {
			run.ForcePause, run.Outcome, run.OutcomeReason = true, "failed", hookErr.Error()
		}
	}
	if ordinaryReservationRetained(task) {
		d.pauseUncertainWorkflow(task, cfg, t, path, rec)
		return
	}
	if task.Err() != nil {
		d.pauseInterruptedWorkflow(task, cfg, t, path, p, run)
		return
	}
	completion := &pipeline.GraphCompletion{NodeID: node.ID, VisitID: t.GraphState.VisitID,
		InvocationID: rec.Invocation.ID, Outcome: run.Outcome, Verdict: run.Verdict, ExitCode: run.Result.ExitCode,
		InfrastructureError: run.ForcePause, Interrupted: run.Result.Interruption != nil || task.Err() != nil,
		SuppressHandoff: hookWarned}
	// For a successful graph agent, classifyGraphExecution sets OutcomeReason
	// to the validated result summary, not the transcript's final message.
	if node.Type == "agent" && run.Outcome == "complete" && run.Reported {
		completion.Summary = run.OutcomeReason
	}
	var after pipeline.GraphAdvance
	if run.Outcome == "waiting" && !run.ForcePause {
		after = d.agentWaitAdvance(t, rec, run)
		run.WaitState = after.State.Wait.Clone()
	} else {
		state := t.GraphState.Clone()
		retainFinishedWait(state)
		after = pipeline.AdvanceGraph(cfg.Pipelines[t.Pipeline].Graph, t.Stage, state, completion)
	}
	if after.Reason != "" && run.OutcomeReason != "" && (run.Outcome != "complete" || run.ForcePause) {
		after.Reason += ": " + run.OutcomeReason
	}
	finish := func(saved *ticket.Ticket) {
		// Recovery invokes this callback after commit, when the task context may already be cancelled.
		d.recordValidatedRun(context.WithoutCancel(root), node.ID, p.agentName, saved.Pipeline, run)
		d.finishWorkflowExit(root, saved, p, node, run)
	}
	saved, err := d.patchFinalExecution(context.WithoutCancel(task), t, path, rec, func(current *ticket.Ticket) error {
		if h := after.State.Handoff; capture != nil && !after.Rejected && h != nil && h.Check != nil && h.InvocationID == rec.Invocation.ID {
			h.Check = capture
		}
		appendRunHistory(current, run, p.agentName, node.ID, p.run)
		if err := d.finishControlAdvance(current, after); err != nil {
			return err
		}
		_ = current.SetField("attempt", 0)
		_ = current.SetField("summary", runSummary(current.Summary, run.FinalMessage))
		if after.Status == ticket.StatusDone {
			return current.SetField("completed_at", time.Now())
		}
		return nil
	}, finish)
	if err != nil {
		log.Error("commit workflow transition", "err", err)
		return
	}
	d.completeFinalization(rec, saved)
}

func freezeWorkflowCheckLog(cfg *config.Config, log *slog.Logger, t *ticket.Ticket, node config.WorkflowNode, run agentRun) *ticket.GraphCheckHandoff {
	if node.Type != "check" || run.ForcePause || !cfg.Pipelines[t.Pipeline].Graph.UsesPreviousHandoff() {
		return nil
	}
	capture := &ticket.GraphCheckHandoff{ExitCode: run.Result.ExitCode}
	// Never fall back to the visit log; it can contain another attempt's output.
	path, err := filepath.Abs(graphLogPath(cfg, t))
	if err != nil {
		log.Warn("resolve workflow check log path", "err", err)
		return capture
	}
	capture.LogPath = path
	if run.Result.LogCaptureError != nil {
		log.Warn("workflow check log capture incomplete", "err", run.Result.LogCaptureError)
		return capture
	}
	capture.Output, capture.OutputAvailable, capture.OutputTruncated = readGraphHandoffOutput(path)
	return capture
}

const graphHandoffOutputLimit = 16 * 1024

func readGraphHandoffOutput(path string) (output string, available, truncated bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", false, false
	}
	data, err := io.ReadAll(io.LimitReader(f, graphHandoffOutputLimit+1))
	if err != nil {
		return "", false, false
	}
	truncated = len(data) > graphHandoffOutputLimit
	if truncated {
		data = data[:graphHandoffOutputLimit]
		// A raw byte limit can land inside a rune. Drop just that suffix;
		// malformed UTF-8 elsewhere is replaced below.
		for start := len(data) - 1; start >= len(data)-utf8.UTFMax+1; start-- {
			if utf8.RuneStart(data[start]) {
				if !utf8.FullRune(data[start:]) {
					data = data[:start]
				}
				break
			}
		}
	}
	output = strings.ToValidUTF8(string(data), "\uFFFD")
	if len(output) > graphHandoffOutputLimit {
		end := graphHandoffOutputLimit
		for !utf8.RuneStart(output[end]) {
			end--
		}
		output = output[:end]
		truncated = true
	}
	return output, true, truncated
}

func (d *Daemon) finishWorkflowExit(root context.Context, saved *ticket.Ticket, p spawnAgentParams, node config.WorkflowNode, run agentRun) {
	if saved.Status == ticket.StatusDone {
		d.removeWorktreeAt(p.log, p.repoPath, p.wtPath)
	}
	if saved.Status == ticket.StatusTodo && root.Err() == nil {
		d.mu.Lock()
		if current, readErr := ticket.ParseFile(p.filePath); readErr == nil && executionOf(current) == executionOf(saved) {
			d.setTicketState(current.ID, current, p.filePath)
			d.enqueue(current)
		}
		d.mu.Unlock()
	}
	if node.Type == "agent" && run.Outcome == "complete" && !run.ForcePause && saved.Status != ticket.StatusTodo && saved.Status != ticket.StatusPaused {
		kind := pipeline.ActionPark
		if saved.Status == ticket.StatusDone {
			kind = pipeline.ActionComplete
		}
		d.startFinalSummary(root, saved, handleExitParams{cfg: p.cfg, log: p.log, ticketID: p.ticketID,
			filePath: p.filePath, agentName: p.agentName, repoPath: p.repoPath, result: run.Result}, kind)
	}
}
