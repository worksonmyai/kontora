package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/worksonmyai/kontora/internal/assistant"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
	"github.com/worksonmyai/kontora/internal/web"
)

//go:embed assistant_extension.js
var assistantExtensionJS string

// assistantMaxConcurrent bounds assistant turns across every thread. It is
// deliberately not max_concurrent_agents: a turn answers a person waiting at
// the pane, and it must neither wait behind the pipeline nor take a worker slot
// away from it.
const assistantMaxConcurrent = 2

// assistantStopWait bounds how long deleting a thread waits for its in-flight
// turn to end before removing the directory anyway.
const assistantStopWait = 5 * time.Second

// assistantGateTimeout is how long a parked write waits for a person before it
// is refused. Past it the agent is told the change was skipped, which is a
// state it can report, rather than being left blocked for the whole turn.
const assistantGateTimeout = 5 * time.Minute

// assistantHookTimeout is what the claude PreToolUse hook is given, in seconds.
// It has to outlast a parked write, or claude kills the hook before the person
// has answered and the answer lands on a call that is already gone.
const assistantHookTimeout = 360

// Environment variables one turn's agent is given so its own tool calls reach
// this daemon rather than editing ticket files behind its back, and so the gate
// can tell its calls from an unrelated local process's.
const (
	assistantURLEnv    = "KONTORA_URL"
	assistantTokenEnv  = "KONTORA_TOKEN"
	assistantThreadEnv = "KONTORA_ASSISTANT_THREAD"
	assistantNonceEnv  = "KONTORA_ASSISTANT_NONCE"
)

// TurnSpawner runs one assistant turn and returns the agent's stdout. It is a
// seam of its own, modelled on FinalSummarySpawner, for the same reason: a turn
// is a headless subprocess with no tmux window, no stage log and no live run,
// and tests drive it without a real agent binary.
type TurnSpawner func(ctx context.Context, p TurnParams) (string, error)

// TurnParams carries the inputs for one turn.
type TurnParams struct {
	Binary  string
	Args    []string
	Dir     string
	Env     map[string]string
	Timeout time.Duration
	// LogFile captures stdout, so a turn that produced nothing useful can still
	// be looked at afterwards.
	LogFile string
	// ThreadID and Kind let a test spawner write a canned session file where
	// the reader will look for it. Kind also names the wire format stdout
	// carries.
	ThreadID string
	Kind     string
	// Stream receives the message the agent is writing. A callback seam rather
	// than an io.Writer, so a test spawner needs no stdout JSON of its own.
	Stream assistant.StreamHandler
}

// WithTurnSpawner overrides the default assistant turn runner.
func WithTurnSpawner(fn TurnSpawner) Option {
	return func(d *Daemon) { d.turnSpawner = fn }
}

// assistantState is the daemon's assistant bookkeeping. One turn runs per
// thread at a time, because the agent session it resumes cannot take two.
type assistantState struct {
	mu sync.Mutex
	// running maps a thread id to the cancel of its in-flight turn.
	running map[string]context.CancelFunc
	// nonces maps a thread id to the secret this turn's agent was given.
	nonces map[string]string
	// done maps a thread id to a channel closed when its turn goroutine has let
	// go of the thread directory.
	done map[string]chan struct{}
	gate *assistant.Gate
	sem  chan struct{}
	// gateLog collects what the gate answered for the turn in flight, per
	// thread, so the turn record carries the audit trail.
	gateLog map[string][]assistant.GateRecord
	// live holds the text each thread's agent is typing right now.
	live *assistant.LiveText
	// writeMu serializes the thread-file update that counts a change.
	writeMu sync.Mutex
}

func newAssistantState() *assistantState {
	return &assistantState{
		running: make(map[string]context.CancelFunc),
		nonces:  make(map[string]string),
		done:    make(map[string]chan struct{}),
		gate:    assistant.NewGate(assistantGateTimeout),
		sem:     make(chan struct{}, assistantMaxConcurrent),
		gateLog: make(map[string][]assistant.GateRecord),
		live:    assistant.NewLiveText(),
	}
}

// claim takes both the thread's slot and one of the global ones. The two
// refusals are told apart: a thread already running a turn and a daemon at its
// global cap are different things to say to someone who just pressed send.
func (a *assistantState) claim(threadID, nonce string, cancel context.CancelFunc) error {
	a.mu.Lock()
	if _, busy := a.running[threadID]; busy {
		a.mu.Unlock()
		return web.ErrAssistantBusy
	}
	a.mu.Unlock()

	select {
	case a.sem <- struct{}{}:
	default:
		return web.ErrAssistantAtCapacity
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Re-checked under the lock: two posts to the same thread can both pass the
	// first check and then both take a global slot.
	if _, busy := a.running[threadID]; busy {
		<-a.sem
		return web.ErrAssistantBusy
	}
	a.running[threadID] = cancel
	a.nonces[threadID] = nonce
	a.done[threadID] = make(chan struct{})
	delete(a.gateLog, threadID)
	a.live.Start(threadID)
	return nil
}

func (a *assistantState) release(threadID string) {
	a.mu.Lock()
	_, held := a.running[threadID]
	done := a.done[threadID]
	delete(a.running, threadID)
	delete(a.nonces, threadID)
	delete(a.done, threadID)
	// Whatever the turn record did not take is from a gate call that outlived
	// the turn, and belongs to nothing.
	delete(a.gateLog, threadID)
	a.mu.Unlock()
	if held {
		<-a.sem
	}
	if done != nil {
		close(done)
	}
	a.gate.Clear(threadID)
	a.live.End(threadID)
}

// recordGate appends what the gate answered for one tool call.
func (a *assistantState) recordGate(threadID string, rec assistant.GateRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gateLog[threadID] = append(a.gateLog[threadID], rec)
}

// takeGates hands the turn's gate records to whoever is writing its record and
// forgets them.
func (a *assistantState) takeGates(threadID string) []assistant.GateRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	recs := a.gateLog[threadID]
	delete(a.gateLog, threadID)
	return recs
}

func (a *assistantState) isRunning(threadID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.running[threadID]
	return ok
}

// stop cancels a thread's in-flight turn. The goroutine running it releases the
// slot on its way out.
func (a *assistantState) stop(threadID string) bool {
	a.mu.Lock()
	cancel, ok := a.running[threadID]
	a.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// stopAndWait cancels a thread's turn and waits for the goroutine running it to
// let go, so a caller that is about to remove the thread directory does not race
// the turn writing its log back into it. The wait is bounded: a spawner that
// ignores its context must not hold the request open.
func (a *assistantState) stopAndWait(threadID string, timeout time.Duration) {
	a.mu.Lock()
	cancel, ok := a.running[threadID]
	done := a.done[threadID]
	a.mu.Unlock()
	if !ok {
		return
	}
	cancel()
	if done == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// stopAll cancels every in-flight turn. A turn runs on a context of its own
// rather than the daemon's, so that a config reload cannot kill one mid-answer;
// shutdown therefore has to end them explicitly, or the wait for background
// work would sit out the whole assistant timeout.
func (a *assistantState) stopAll() {
	a.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(a.running))
	for _, cancel := range a.running {
		cancels = append(cancels, cancel)
	}
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// nonceMatches reports whether a gate call carries the secret this thread's
// current turn was started with. Without it any local process could approve its
// own writes against a tokenless loopback daemon.
func (a *assistantState) nonceMatches(threadID, nonce string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	want, ok := a.nonces[threadID]
	return ok && want != "" && want == nonce
}

// assistantStore returns the thread store for the config in effect. It is built
// per call rather than held, so a reload that moves logs_dir is followed.
func (d *Daemon) assistantStore(cfg *config.Config) *assistant.Store {
	return assistant.NewStore(filepath.Join(expandTilde(cfg.LogsDir), "assistant"))
}

// assistantAgent resolves the configured assistant agent, or reports that the
// section names none.
func assistantAgent(cfg *config.Config) (config.Agent, error) {
	if !cfg.Assistant.Enabled() {
		return config.Agent{}, web.ErrAssistantDisabled
	}
	agentCfg, ok := cfg.Agents[cfg.Assistant.Agent]
	if !ok {
		return config.Agent{}, fmt.Errorf("%w: agent %q is not defined", web.ErrAssistantDisabled, cfg.Assistant.Agent)
	}
	if agentCfg.Kind() == "" {
		return config.Agent{}, fmt.Errorf("%w: agent %q runs %s, and the assistant only supports claude and pi", web.ErrAssistantDisabled, cfg.Assistant.Agent, agentCfg.Binary)
	}
	return agentCfg, nil
}

// assistantThreadAgent resolves the agent one thread runs on. A thread is one
// agent session, so it keeps running on the agent it was created with: pointing
// assistant.agent somewhere else must not resume a claude session with pi, which
// would start a fresh session while the pane still read the old transcript.
func assistantThreadAgent(cfg *config.Config, thread assistant.Thread) (config.Agent, error) {
	if !cfg.Assistant.Enabled() {
		return config.Agent{}, web.ErrAssistantDisabled
	}
	agentCfg, ok := cfg.Agents[thread.Agent]
	if !ok {
		return config.Agent{}, fmt.Errorf("%w: this chat runs on agent %q, which is no longer defined; start a new chat", web.ErrAssistantStale, thread.Agent)
	}
	if agentCfg.Kind() != thread.Kind {
		return config.Agent{}, fmt.Errorf("%w: this chat runs on %s, and agent %q now runs %s; start a new chat", web.ErrAssistantStale, thread.Kind, thread.Agent, agentCfg.Binary)
	}
	return agentCfg, nil
}

// AssistantConfig reports whether the pane can be used and, when it cannot, why
// not in the words the configure hint shows.
func (d *Daemon) AssistantConfig() web.AssistantConfigInfo {
	cfg := d.config()
	agentCfg, err := assistantAgent(cfg)
	if err != nil {
		// A config that names an agent and still fails says why; one that names
		// none says what to set, because there is nothing wrong to report.
		hint := "Set assistant.agent to one of your claude or pi agents to use the assistant."
		if cfg.Assistant.Enabled() {
			hint = err.Error()
		}
		return web.AssistantConfigInfo{Enabled: false, Hint: hint}
	}
	return web.AssistantConfigInfo{
		Enabled:  true,
		Agent:    cfg.Assistant.Agent,
		Kind:     agentCfg.Kind(),
		Model:    cfg.Assistant.Model,
		Autonomy: cfg.Assistant.Autonomy,
		Workdir:  expandTilde(cfg.Assistant.Workdir),
	}
}

// ListAssistantThreads returns the chat history, most recently used first.
func (d *Daemon) ListAssistantThreads() ([]web.AssistantThreadInfo, error) {
	cfg := d.config()
	threads, err := d.assistantStore(cfg).List()
	if err != nil {
		return nil, err
	}
	out := make([]web.AssistantThreadInfo, 0, len(threads))
	for _, t := range threads {
		out = append(out, d.assistantThreadInfo(t, nil))
	}
	return out, nil
}

// CreateAssistantThread opens a chat. Its agent, model and working directory
// are fixed here: a thread is one agent session, and claude keys its session
// files by working directory, so neither can move between turns.
func (d *Daemon) CreateAssistantThread(req web.CreateAssistantThreadRequest) (web.AssistantThreadInfo, error) {
	cfg := d.config()
	agentCfg, err := assistantAgent(cfg)
	if err != nil {
		return web.AssistantThreadInfo{}, err
	}
	autonomy := req.Autonomy
	if !slices.Contains(config.Autonomies, autonomy) {
		autonomy = cfg.Assistant.Autonomy
	}
	now := time.Now().UTC()
	thread := assistant.Thread{
		ID:        assistant.NewID(),
		Title:     "New chat",
		CreatedAt: now,
		UpdatedAt: now,
		Agent:     cfg.Assistant.Agent,
		Kind:      agentCfg.Kind(),
		Model:     cfg.Assistant.Model,
		Effort:    cfg.Assistant.Effort,
		Cwd:       expandTilde(cfg.Assistant.Workdir),
		Autonomy:  autonomy,
	}
	if err := d.assistantStore(cfg).Save(thread); err != nil {
		return web.AssistantThreadInfo{}, err
	}
	return d.assistantThreadInfo(thread, []web.AssistantMessage{}), nil
}

// GetAssistantThread returns one thread with the messages posted to it.
func (d *Daemon) GetAssistantThread(id string) (web.AssistantThreadInfo, error) {
	cfg := d.config()
	store := d.assistantStore(cfg)
	thread, err := store.Load(id)
	if err != nil {
		return web.AssistantThreadInfo{}, assistantErr(err)
	}
	return d.assistantThreadInfo(thread, assistantMessages(store, id)), nil
}

// DeleteAssistantThread drops a thread and its transcript.
func (d *Daemon) DeleteAssistantThread(id string) error {
	d.assistant.stopAndWait(id, assistantStopWait)
	d.assistant.live.Clear(id)
	return assistantErr(d.assistantStore(d.config()).Delete(id))
}

// StopAssistantTurn cancels the turn a thread is running.
func (d *Daemon) StopAssistantTurn(id string) error {
	if _, err := d.assistantStore(d.config()).Load(id); err != nil {
		return assistantErr(err)
	}
	d.assistant.stop(id)
	return nil
}

// AssistantActivity returns the thread's transcript as the same tape a ticket
// run's activity tab reads, sliced at the caller's cursor and never past the
// point the agent may still rewrite. Any write waiting on the person rides the
// same response, so one poll drives the tool rows and the proposal card.
func (d *Daemon) AssistantActivity(q web.AssistantActivityQuery) (web.AssistantActivityInfo, error) {
	cfg := d.config()
	store := d.assistantStore(cfg)
	thread, err := store.Load(q.ID)
	if err != nil {
		return web.AssistantActivityInfo{}, assistantErr(err)
	}

	info := web.AssistantActivityInfo{
		Running:  d.assistant.isRunning(q.ID),
		Autonomy: thread.Autonomy,
		Messages: assistantMessages(store, q.ID),
	}
	if pending, ok := d.assistant.gate.Pending(q.ID); ok {
		info.Gate = &pending
	}
	// Before the ETag, so the validator and what it validates describe the same
	// text.
	partial := d.assistant.live.Snapshot(q.ID)
	info.Partial = partial.Text
	info.PartialGen = partial.Gen
	info.PartialTool = partial.Tool

	path := d.assistantSessionPath(cfg, thread)
	if path == "" {
		// The turn has started but the agent has written nothing yet. The tape
		// comes from the parser so it carries the version and agent fields the
		// client reads from the first poll on.
		tape := emptyTape(thread.Kind == config.AgentKindPi)
		info.Tape = &tape
		return info, nil
	}

	// Everything the response carries that is not in the session file is folded
	// into the validator: the parked call, the turn records and whether a turn
	// is still running. Without them a turn that ends without writing the
	// session file again answers 304 for ever, and the pane sticks on "working".
	info.ETag = assistantETag(path, thread.Turns, info)
	if info.ETag != "" && info.ETag == q.IfNoneMatch {
		return web.AssistantActivityInfo{NotModified: true, ETag: info.ETag}, nil
	}

	tape, err := sessionTape(path, thread.Kind == config.AgentKindPi)
	if err != nil {
		empty := emptyTape(thread.Kind == config.AgentKindPi)
		info.Tape = &empty
		return info, nil //nolint:nilerr // a session file that will not read is an empty tape, not a failed request.
	}
	info.Tape = &tape
	info.Offset = tape.SliceAt(q.After)

	if info.Partial != "" && assistantPartialLanded(tape, info.Partial, partial.Sealed, partial.Truncated) {
		// Removal and arrival ride one response, so the pane swaps typed text
		// for settled in a single update: no duplicate, no frame with neither.
		if partial.Sealed {
			d.assistant.live.Suppress(q.ID, partial.Gen)
		}
		info.Partial = ""
		info.PartialTool = ""
		info.ETag = assistantETag(path, thread.Turns, info)
		// The validator compared above still had the partial folded in, so an
		// unsealed block that keeps landing would re-send this body every poll.
		if info.ETag != "" && info.ETag == q.IfNoneMatch {
			return web.AssistantActivityInfo{NotModified: true, ETag: info.ETag}, nil
		}
	}
	return info, nil
}

// assistantPartialLanded reads the tape, so it assumes nothing about the order
// claude writes its stdout lines in. A sealed block also matches a tape entry
// that merely starts with it, covering whitespace the API strips and a cut-off
// at PartialMax; a live one is matched exactly, or a message repeating the last
// one's opening characters would blank for its first frames.
func assistantPartialLanded(tape logfmt.Tape, partial string, sealed, truncated bool) bool {
	if partial == "" {
		return false
	}
	for _, ev := range slices.Backward(tape.Events) {
		if ev.Kind != "text" {
			continue
		}
		text := ev.Text
		if text == partial {
			return true
		}
		if !sealed || !strings.HasPrefix(text, partial) {
			return false
		}
		// The tail decides. Padding or a cut-off at PartialMax is the same
		// message; anything else is an earlier one that happens to open the
		// same way, and suppressing on it would lose the reply for good.
		return truncated || strings.TrimSpace(text[len(partial):]) == ""
	}
	return false
}

// AssistantPartial answers from memory while the thread has anything to report:
// the stream reads it ten times a second, and a thread.json read at that rate is
// what the stream exists to avoid. Only a thread with no buffer is looked up on
// disk, to tell one that has said nothing from one that does not exist.
func (d *Daemon) AssistantPartial(id string) (web.AssistantPartialInfo, error) {
	p := d.assistant.live.Snapshot(id)
	if !p.Present {
		if _, err := d.assistantStore(d.config()).Load(id); err != nil {
			return web.AssistantPartialInfo{}, assistantErr(err)
		}
	}
	return web.AssistantPartialInfo{
		Running: d.assistant.isRunning(id),
		Gen:     p.Gen,
		Text:    p.Text,
		Tool:    p.Tool,
		Sealed:  p.Sealed,
	}, nil
}

// assistantETag is the validator one poll answers with. fileETag alone would
// miss everything the response carries that is not in the session file.
func assistantETag(path string, turns int, info web.AssistantActivityInfo) string {
	base := fileETag(path, turns)
	if base == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(base)
	fmt.Fprintf(&b, "|running=%t|messages=%d", info.Running, len(info.Messages))
	if n := len(info.Messages); n > 0 {
		last := info.Messages[n-1]
		fmt.Fprintf(&b, "|%d|%s", last.At.UnixNano(), last.Error)
	}
	if info.Gate != nil {
		b.WriteString("|" + info.Gate.ID)
	}
	// Length, not content: within a generation the text only grows. Without it
	// the poll answers 304 for a whole message that produces no tape rows.
	fmt.Fprintf(&b, "|partial=%d,%d,%s", info.PartialGen, len(info.Partial), info.PartialTool)
	return web.ContentETag([]byte(b.String()), "")
}

// assistantSessionPath locates the JSONL this thread's agent writes to. A
// claude thread's lives in claude's own config directory, which the agent
// environment names; a pi thread's lives under the thread directory.
func (d *Daemon) assistantSessionPath(cfg *config.Config, thread assistant.Thread) string {
	if thread.SessionID == "" && thread.Kind == config.AgentKindClaude {
		return ""
	}
	var claudeConfigDir string
	if agentCfg, ok := cfg.Agents[thread.Agent]; ok {
		claudeConfigDir = session.ClaudeConfigDir(agentEnv(cfg, agentCfg, d.configPath, thread.Agent, ""))
	} else {
		claudeConfigDir = session.ClaudeConfigDir(nil)
	}
	piDir, err := d.assistantStore(cfg).PiSessionDir(thread.ID)
	if err != nil {
		piDir = ""
	}
	return assistant.SessionPath(thread.Kind, thread.SessionID, claudeConfigDir, piDir)
}

// PostAssistantMessage starts a turn. It returns as soon as the subprocess is
// under way: the reply arrives through the activity poll, not this response.
func (d *Daemon) PostAssistantMessage(id string, req web.AssistantMessageRequest) error {
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return fmt.Errorf("%w: the message is empty", web.ErrInvalidConfig)
	}
	cfg := d.config()
	store := d.assistantStore(cfg)
	thread, err := store.Load(id)
	if err != nil {
		return assistantErr(err)
	}
	agentCfg, err := assistantThreadAgent(cfg, thread)
	if err != nil {
		return err
	}
	if slices.Contains(config.Autonomies, req.Autonomy) {
		thread.Autonomy = req.Autonomy
	}
	if thread.SessionID == "" {
		thread.SessionID = newSessionID()
	}
	// The session file on disk decides, not the turn counter. A turn that dies
	// before the agent writes one leaves nothing to resume, and claude answers
	// -r on a session it never created with "No conversation found", which
	// would fail every later message in the thread.
	resume := thread.Turns > 0 && d.assistantSessionPath(cfg, thread) != ""
	thread.Turns++
	if thread.Turns == 1 {
		thread.Title = assistant.Title(text)
	}
	thread.UpdatedAt = time.Now().UTC()

	nonce, err := assistantNonce()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := d.assistant.claim(id, nonce, cancel); err != nil {
		cancel()
		return err
	}
	// Written before the turn starts, so the gate and the poll both read the
	// autonomy and the session this turn actually runs with.
	if err := store.Save(thread); err != nil {
		d.assistant.release(id)
		cancel()
		return err
	}

	d.background.Go(func() {
		defer cancel()
		d.runAssistantTurn(ctx, cfg, agentCfg, store, thread, text, req.Context, resume)
	})
	return nil
}

// assistantBoard describes the configured board as the lines the system brief
// renders. A section with nothing to state is left out rather than rendered
// empty: a board with no projects should not tell the agent it has none.
func assistantBoard(cfg *config.Config) []string {
	var lines []string

	if len(cfg.Pipelines) > 0 {
		names := slices.Sorted(maps.Keys(cfg.Pipelines))
		parts := make([]string, 0, len(names))
		for _, name := range names {
			stages := make([]string, 0, len(cfg.Pipelines[name]))
			for _, step := range cfg.Pipelines[name] {
				stages = append(stages, step.Stage)
			}
			parts = append(parts, fmt.Sprintf("%s (%s)", name, strings.Join(stages, " -> ")))
		}
		lines = append(lines, "Pipelines: "+strings.Join(parts, ", "))
	}

	if len(cfg.Agents) > 0 {
		names := slices.Sorted(maps.Keys(cfg.Agents))
		parts := make([]string, 0, len(names))
		for _, name := range names {
			if name == cfg.DefaultAgent {
				name += " (default)"
			}
			parts = append(parts, name)
		}
		lines = append(lines, "Agents: "+strings.Join(parts, ", "))
	}

	if len(cfg.Statuses) > 0 {
		lines = append(lines, "Custom statuses: "+strings.Join(cfg.Statuses, ", "))
	}

	if len(cfg.Projects) > 0 {
		names := slices.Sorted(maps.Keys(cfg.Projects))
		parts := make([]string, 0, len(names))
		for _, name := range names {
			parts = append(parts, fmt.Sprintf("%s (%s)", name, cfg.Projects[name].Path))
		}
		lines = append(lines, "Projects: "+strings.Join(parts, ", "))
	}

	return lines
}

// assistantCounts counts the tickets the daemon holds in memory, per status, in
// board order, then any other status a ticket carries. A status with no tickets
// is left out, so the line says what is there rather than listing zeroes.
func (d *Daemon) assistantCounts(cfg *config.Config) []string {
	counts := map[string]int{}
	d.mu.Lock()
	for _, ts := range d.tickets {
		counts[string(ts.ticket.Status)]++
	}
	d.mu.Unlock()

	order := append(slices.Clone(config.BoardStatusOrder), cfg.Statuses...)
	var extra []string
	for status := range counts {
		if !slices.Contains(order, status) {
			extra = append(extra, status)
		}
	}
	slices.Sort(extra)
	order = append(order, extra...)

	var lines []string
	for _, status := range order {
		if n := counts[status]; n > 0 {
			lines = append(lines, fmt.Sprintf("%d %s", n, status))
		}
	}
	return lines
}

// assistantPageLines splits the posted page context into the lines the brief
// renders. Blank lines are dropped, so a client that pads its payload does not
// pad the prompt.
func assistantPageLines(raw string) []string {
	var lines []string
	for line := range strings.SplitSeq(raw, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// runAssistantTurn spawns one headless agent process and records what it did.
func (d *Daemon) runAssistantTurn(ctx context.Context, cfg *config.Config, agentCfg config.Agent, store *assistant.Store, thread assistant.Thread, text, pageContext string, resume bool) {
	defer d.assistant.release(thread.ID)

	log := d.log.With("assistant_thread", thread.ID, "turn", thread.Turns)
	turn := assistant.Turn{N: thread.Turns, Text: text, Context: pageContext, StartedAt: time.Now().UTC()}

	finish := func(err error) {
		turn.EndedAt = time.Now().UTC()
		turn.Gates = d.assistant.takeGates(thread.ID)
		if err != nil {
			turn.Error = err.Error()
			turn.ExitCode = 1
			log.Warn("assistant turn failed", "err", err)
		}
		// Reloaded first: a thread deleted while its turn ran is gone, and
		// writing the turn record would leave the directory behind.
		latest, loadErr := store.Load(thread.ID)
		if loadErr != nil {
			return
		}
		if err := store.AppendTurn(thread.ID, turn); err != nil {
			log.Warn("recording the assistant turn failed", "err", err)
		}
		latest.UpdatedAt = time.Now().UTC()
		if err := store.Save(latest); err != nil {
			log.Warn("updating the assistant thread failed", "err", err)
		}
	}

	env := d.assistantEnv(cfg, agentCfg, thread.ID)
	gateFile, err := d.writeAssistantGateFile(agentCfg.Kind(), env)
	if err != nil {
		finish(fmt.Errorf("writing the tool gate: %w", err))
		return
	}
	defer os.Remove(gateFile)

	spec := assistant.TurnSpec{
		Prompt:    text,
		SessionID: thread.SessionID,
		Resume:    resume,
		GateFile:  gateFile,
		SystemPrompt: assistant.SystemPrompt(cfg.Assistant.Prompt, assistant.PromptData{
			Autonomy:     thread.Autonomy,
			Cwd:          thread.Cwd,
			TicketsDir:   expandTilde(cfg.TicketsDir),
			LogsDir:      expandTilde(cfg.LogsDir),
			WorktreesDir: expandTilde(cfg.WorktreesDir),
			Board:        assistantBoard(cfg),
			Counts:       d.assistantCounts(cfg),
			PageContext:  assistantPageLines(turn.Context),
		}),
	}
	switch agentCfg.Kind() {
	case config.AgentKindClaude:
		// The thread runs in the tickets dir, so the run logs and the worktrees
		// have to be named or claude refuses to read them.
		spec.AddDirs = []string{expandTilde(cfg.LogsDir), expandTilde(cfg.WorktreesDir)}
	case config.AgentKindPi:
		dir, err := store.PiSessionDir(thread.ID)
		if err != nil {
			finish(err)
			return
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			finish(err)
			return
		}
		spec.SessionDir = dir
	}
	var stream assistant.StreamHandler
	if cfg.Assistant.StreamEnabled() {
		spec.Stream = true
		live, id := d.assistant.live, thread.ID
		stream = assistant.StreamHandler{
			Block: func() { live.Block(id) },
			Text:  func(delta string) { live.Append(id, delta) },
			Tool:  func(name string) { live.Tool(id, name) },
			Seal:  func() { live.Seal(id) },
		}
		log.Debug("assistant turn asks for partial messages", "kind", agentCfg.Kind())
	}

	args, err := assistant.BuildArgs(agentCfg, thread.Model, thread.Effort, spec)
	if err != nil {
		finish(err)
		return
	}
	binary, err := d.agentLookup(agentCfg.Binary)
	if err != nil {
		finish(fmt.Errorf("agent %q: %w", thread.Agent, err))
		return
	}

	logFile, err := store.TurnLogPath(thread.ID, thread.Turns)
	if err != nil {
		finish(err)
		return
	}
	run := func(args []string, stream assistant.StreamHandler) error {
		_, err := d.turnSpawner(ctx, TurnParams{
			Binary:   binary,
			Args:     args,
			Dir:      thread.Cwd,
			Env:      env,
			Timeout:  cfg.Assistant.Timeout.Duration,
			LogFile:  logFile,
			ThreadID: thread.ID,
			Kind:     agentCfg.Kind(),
			Stream:   stream,
		})
		return err
	}

	err = run(args, stream)
	// An agent that predates the flag rejects it before it reads the prompt, so
	// every turn would fail until someone found assistant.stream. The turn is
	// worth more than the streaming, so it is run again without it.
	streamFlag := assistant.StreamFlagFor(agentCfg.Kind())
	if err != nil && spec.Stream && ctx.Err() == nil && streamFlag != "" && strings.Contains(err.Error(), streamFlag) {
		log.Warn("this agent does not know the partial-message flag; running the turn without it",
			"kind", agentCfg.Kind(), "flag", streamFlag, "hint", "set assistant.stream: false to stop asking")
		spec.Stream = false
		retryArgs, buildErr := assistant.BuildArgs(agentCfg, thread.Model, thread.Effort, spec)
		if buildErr == nil {
			err = run(retryArgs, assistant.StreamHandler{})
		}
	}
	finish(err)
}

// assistantEnv is the agent environment plus what the turn needs on top: the
// daemon's own address and token, so the agent's `kontora` calls go through the
// API the pane is watching rather than editing ticket files behind it, and the
// thread and nonce the gate authenticates a tool call with.
func (d *Daemon) assistantEnv(cfg *config.Config, agentCfg config.Agent, threadID string) map[string]string {
	env := agentEnv(cfg, agentCfg, d.configPath, cfg.Assistant.Agent, "")
	if addr := d.webAddr.Load(); addr != nil && *addr != "" {
		env[assistantURLEnv] = "http://" + assistantLoopbackAddr(*addr)
	}
	if cfg.Web.Token != "" {
		env[assistantTokenEnv] = cfg.Web.Token
	}
	env[assistantThreadEnv] = threadID
	d.assistant.mu.Lock()
	env[assistantNonceEnv] = d.assistant.nonces[threadID]
	d.assistant.mu.Unlock()
	// A turn is exec'd directly, with no login shell to rebuild PATH the way
	// the tmux wrapper of a ticket run gets one.
	return withCommonPath(env)
}

// assistantLoopbackAddr rewrites a wildcard bind to loopback. The agent reaches
// the daemon over its own listener, and a Host header of 0.0.0.0 is neither
// loopback nor an allowed host, so with `web.host: 0.0.0.0` every gate call and
// every `kontora` call the agent makes would answer 403.
func assistantLoopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !web.UnspecifiedHost(host) {
		return addr
	}
	if strings.Contains(host, ":") {
		return net.JoinHostPort("::1", port)
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// writeAssistantGateFile writes the per-turn file that routes the agent's tool
// calls through the gate: a settings JSON with a PreToolUse hook for claude, an
// extension registering a tool_call handler for pi. The caller removes it.
func (d *Daemon) writeAssistantGateFile(kind string, env map[string]string) (string, error) {
	switch kind {
	case config.AgentKindClaude:
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("locating the kontora binary the hook runs: %w", err)
		}
		hook := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{
					"matcher": "",
					"hooks": []any{map[string]any{
						"type":    "command",
						"command": shellQuote(self) + " assistant-gate",
						// Longer than a parked write waits, or claude kills the
						// hook while the person is still deciding.
						"timeout": assistantHookTimeout,
					}},
				}},
			},
		}
		data, err := json.Marshal(hook)
		if err != nil {
			return "", err
		}
		return writeTempFile("kontora-assistant-settings-*.json", string(data))
	case config.AgentKindPi:
		return writeTempFile("kontora-assistant-ext-*.js", renderAssistantExtension(env))
	}
	return "", fmt.Errorf("agent kind %q has no tool gate", kind)
}

// renderAssistantExtension fills in what the pi gate needs to reach the daemon.
// The values are substituted rather than read from the environment: the agent's
// own tools can read that environment, and the nonce is exactly what stops
// something else approving this turn's writes.
func renderAssistantExtension(env map[string]string) string {
	s := assistantExtensionJS
	for placeholder, key := range map[string]string{
		"__KONTORA_URL__":    assistantURLEnv,
		"__KONTORA_TOKEN__":  assistantTokenEnv,
		"__KONTORA_THREAD__": assistantThreadEnv,
		"__KONTORA_NONCE__":  assistantNonceEnv,
	} {
		s = strings.ReplaceAll(s, placeholder, jsStringEscape(env[key]))
	}
	return s
}

// jsStringEscape makes a value safe inside the double-quoted literals above. The
// four are a URL, a token, an id and a hex nonce, so this is belt and braces
// rather than a live injection path, but a token with a quote in it would
// otherwise produce an extension that does not parse.
func jsStringEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "<", `\x3c`)
	return r.Replace(s)
}

// shellQuote wraps a path in single quotes, because claude runs a hook command
// through a shell and an installation path with a space in it would otherwise
// be read as two words.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeTempFile writes content to a new temporary file and returns its path. A
// write or close error removes the partial file.
func writeTempFile(pattern, content string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// AskAssistantGate answers one intercepted tool call. It is the agent side of
// the gate: the claude hook and the pi extension both reach it, and it blocks
// while a write waits for the person at the pane.
func (d *Daemon) AskAssistantGate(req web.AssistantGateAskRequest) (web.AssistantGateAskResponse, error) {
	if !d.assistant.nonceMatches(req.Thread, req.Nonce) {
		return web.AssistantGateAskResponse{}, web.ErrAssistantGateDenied
	}
	thread, err := d.assistantStore(d.config()).Load(req.Thread)
	if err != nil {
		return web.AssistantGateAskResponse{}, assistantErr(err)
	}

	kind := assistant.Classify(req.Tool, req.Input)
	verdict := assistant.Resolve(thread.Autonomy, kind)
	call := assistant.Call{
		ThreadID: req.Thread,
		Tool:     req.Tool,
		Arg:      assistantCallArg(req.Input),
		Kind:     kind,
	}

	record := func(approved bool) {
		d.assistant.recordGate(req.Thread, assistant.GateRecord{
			Tool:     req.Tool,
			Arg:      call.Arg,
			Kind:     kind,
			Verdict:  verdict,
			Approved: approved,
		})
	}

	switch verdict {
	case assistant.VerdictAllow:
		record(true)
		if kind != assistant.DecisionRead {
			d.recordAssistantWrite(req.Thread)
		}
		return web.AssistantGateAskResponse{Allow: true}, nil
	case assistant.VerdictDeny:
		record(false)
		return web.AssistantGateAskResponse{Allow: false, Reason: assistant.DenyReason(kind, thread.Autonomy)}, nil
	case assistant.VerdictPark:
		_, done := d.assistant.gate.Park(call)
		approved := <-done
		record(approved)
		if approved {
			d.recordAssistantWrite(req.Thread)
			return web.AssistantGateAskResponse{Allow: true}, nil
		}
		return web.AssistantGateAskResponse{Allow: false, Reason: assistant.DenyReason(kind, thread.Autonomy)}, nil
	}
	return web.AssistantGateAskResponse{}, fmt.Errorf("unknown gate verdict %q", verdict)
}

// ResolveAssistantGate is the person's answer to a parked write.
func (d *Daemon) ResolveAssistantGate(gateID string, approve bool) error {
	if !d.assistant.gate.Resolve(gateID, approve) {
		return web.ErrAssistantGateNotFound
	}
	return nil
}

// recordAssistantWrite counts a change the thread made, so the history row can
// say how much a chat did without reading its whole transcript.
func (d *Daemon) recordAssistantWrite(threadID string) {
	// Read-modify-write on one file, and in auto mode two tool calls reach it
	// at once, so the increment is serialized rather than lost.
	d.assistant.writeMu.Lock()
	defer d.assistant.writeMu.Unlock()
	store := d.assistantStore(d.config())
	thread, err := store.Load(threadID)
	if err != nil {
		return
	}
	thread.Writes++
	thread.UpdatedAt = time.Now().UTC()
	if err := store.Save(thread); err != nil {
		d.log.Warn("recording an assistant write failed", "assistant_thread", threadID, "err", err)
	}
}

// assistantCallArg is the one-line form of a tool call the proposal card shows.
// Only the fields a call actually carries are read, so an unknown tool shows its
// name and nothing invented.
func assistantCallArg(input map[string]any) string {
	for _, key := range []string{"command", "file_path", "path", "pattern", "url"} {
		if v, ok := input[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// assistantMessages reads the thread's turn log for the pane, which shows what
// was asked alongside the agent's own tape.
func assistantMessages(store *assistant.Store, id string) []web.AssistantMessage {
	turns, err := store.Turns(id)
	if err != nil {
		return []web.AssistantMessage{}
	}
	out := make([]web.AssistantMessage, 0, len(turns))
	for _, t := range turns {
		out = append(out, web.AssistantMessage{N: t.N, Text: t.Text, At: t.StartedAt, Error: t.Error})
	}
	return out
}

func (d *Daemon) assistantThreadInfo(t assistant.Thread, messages []web.AssistantMessage) web.AssistantThreadInfo {
	return web.AssistantThreadInfo{
		ID:        t.ID,
		Title:     t.Title,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
		Agent:     t.Agent,
		Kind:      t.Kind,
		Model:     t.Model,
		Autonomy:  t.Autonomy,
		Turns:     t.Turns,
		Writes:    t.Writes,
		Running:   d.assistant.isRunning(t.ID),
		Messages:  messages,
	}
}

// assistantErr maps the store's own not-found onto the one the web layer turns
// into a 404.
func assistantErr(err error) error {
	if errors.Is(err, assistant.ErrThreadNotFound) {
		return web.ErrAssistantNotFound
	}
	return err
}

func assistantNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// defaultTurnSpawner runs the agent as a plain subprocess. Its stdout is both
// returned and written to the turn log: the transcript the pane reads comes
// from the agent's own session file, so stdout is only ever for debugging.
func defaultTurnSpawner(ctx context.Context, p TurnParams) (string, error) {
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}
	var stdout, stderr bytes.Buffer
	// The tee, not one arm of an io.MultiWriter: the same parse that finds the
	// deltas decides which lines the turn log keeps.
	var out io.Writer = &stdout
	var sw *assistant.StreamWriter
	if p.Stream.Live() {
		sw = assistant.NewStreamWriter(p.Kind, &stdout, p.Stream)
		out = sw
	}
	result, err := process.Run(ctx, process.RunParams{
		Binary: p.Binary,
		Args:   p.Args,
		Dir:    p.Dir,
		Stdout: out,
		Stderr: &stderr,
		Env:    envPairs(p.Env),
	})
	if sw != nil {
		// cmd.Wait joined the copy goroutine, so nothing is still writing.
		_ = sw.Close()
	}
	// The thread directory was made when the thread was, so the log is only
	// written when it is still there: a thread deleted mid-turn must not come
	// back as a directory holding one log and no thread.json.
	if p.LogFile != "" {
		if st, statErr := os.Stat(filepath.Dir(p.LogFile)); statErr == nil && st.IsDir() {
			_ = os.WriteFile(p.LogFile, stdout.Bytes(), 0o644)
		}
	}
	if err != nil {
		return stdout.String(), err
	}
	agent := filepath.Base(p.Binary)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout.String(), fmt.Errorf("%s: %w", agent, ctxErr)
	}
	if result.ExitCode != 0 {
		return stdout.String(), fmt.Errorf("%s exited with code %d: %s", agent, result.ExitCode, truncateSummary(strings.TrimSpace(stderr.String())))
	}
	// pi's json mode exits 0 on a turn its own text mode would have failed, so
	// the only account of the failure is the one the decoder read off the wire.
	if sw != nil {
		if failure := sw.Failure(); failure != nil {
			return stdout.String(), fmt.Errorf("%s: %s", agent, truncateSummary(failure.Error()))
		}
	}
	return stdout.String(), nil
}
