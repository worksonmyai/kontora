// Package compaction provides an offline estimator for ticket-phase checkpoint
// compaction in pi sessions. It reads completed pi JSONL session files,
// detects ticket-phase boundaries, calibrates from recorded compactions, and
// projects token reduction scenarios at various thresholds.
//
// The estimator is read-only: it never writes to any session, sidecar, ticket,
// or configuration file.
package compaction

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ExclusionReason explains why a session was excluded from counterfactual
// projection.
type ExclusionReason string

const (
	ExcludeBranchSummary ExclusionReason = "branch_summary"
	ExcludeNoUsage       ExclusionReason = "no_usage"
	ExcludeUnreadable    ExclusionReason = "unreadable"
	ExcludeBrokenChain   ExclusionReason = "broken_chain"
	ExcludeHasCompaction ExclusionReason = "has_compaction"
	ExcludeLiveResume    ExclusionReason = "live_resume"
)

// AssistantCall is one usage-bearing assistant entry on the active path.
type AssistantCall struct {
	ID string
	// PromptContext is input + cacheRead + cacheWrite.
	PromptContext int
	Output        int
}

// Checkpoint is a detected ticket-phase boundary on the active path.
// AfterCallIndex is the index into SessionTrace.AssistantCalls of the last
// assistant call at or before this checkpoint. A compaction decision is
// evaluated after that call completes.
type Checkpoint struct {
	AfterCallIndex int
	CompletedPhase string
	NextPhase      string
	// Source is "todo" for manage_todo_list detection or "explicit" for
	// kontora-phase-checkpoint custom entries.
	Source  string
	Outcome string
}

// Record holds calibration data from an observed compaction.
type Record struct {
	TokensBefore      int
	SummaryInput      int
	SummaryOutput     int
	PostCompactionCtx int  // first assistant prompt context after compaction; 0 if absent
	HasPostCompaction bool // whether a post-compaction assistant call was found
}

// SessionTrace is the parsed active path of one pi session.
type SessionTrace struct {
	File           string
	Ticket         string
	Stage          string
	SessionID      string
	AssistantCalls []AssistantCall
	Checkpoints    []Checkpoint
	Compactions    []Record
	Exclusion      ExclusionReason // empty if eligible
	TotalPrompt    int             // sum of all assistant PromptContext on active path
	TotalOutput    int             // sum of all assistant Output on active path
}

// Calibration holds statistics derived from observed compactions.
type Calibration struct {
	SampleCount       int
	SummaryRatios     []float64 // summary input / tokensBefore, sorted
	SummaryOutputs    []int     // sorted
	PostCompactionCtx []int     // sorted; only from records where HasPostCompaction
}

// CalibrationScenario holds one set of calibrated parameters.
type CalibrationScenario struct {
	Label             string
	SummaryRatio      float64
	SummaryOutput     int
	PostCompactionCtx int
}

// ThresholdResult is the projected outcome for one threshold value.
type ThresholdResult struct {
	Threshold int
	Scenarios [3]ScenarioResult // low, median, high
}

// ScenarioResult is the projection for one calibration scenario at one
// threshold.
type ScenarioResult struct {
	Label             string
	CompactionCount   int
	ProjectedPrompt   int
	ProjectedOutput   int
	ProjectedSummary  int // summary input + output added by compactions
	BaselinePrompt    int
	BaselineOutput    int
	PromptReduction   int // baseline prompt - projected prompt
	RawTokenReduction int // prompt reduction - summary overhead
}

// TopSessionResult identifies one session in the top-N list, ranked by
// projected median raw-token reduction at the lowest configured threshold.
type TopSessionResult struct {
	Ticket          string
	Stage           string
	File            string
	BaselineTokens  int
	Checkpoints     int
	MedianReduction int
}

// Report is the aggregate output of the estimator.
type Report struct {
	FilesScanned int
	Eligible     int
	Excluded     map[ExclusionReason]int
	Checkpoints  int // total detected across eligible sessions
	Calibration  Calibration
	Thresholds   []ThresholdResult
	TopSessions  []TopSessionResult
}

// SessionReader abstracts access to a session file for testability.
type SessionReader interface {
	Path() string
	Open() (io.Reader, error)
	Close() error
}

// SessionInfo is an optional interface that SessionReader implementations can
// satisfy to carry ticket/stage metadata and live-resume state discovered from
// the directory layout.
type SessionInfo interface {
	Ticket() string
	Stage() string
	IsLive() bool
}

// sessionFile implements SessionReader and SessionInfo for files on disk.
type sessionFile struct {
	path   string
	ticket string
	stage  string
	live   bool
	f      *os.File
}

func (s *sessionFile) Path() string             { return s.path }
func (s *sessionFile) Ticket() string           { return s.ticket }
func (s *sessionFile) Stage() string            { return s.stage }
func (s *sessionFile) IsLive() bool             { return s.live }
func (s *sessionFile) Open() (io.Reader, error) { f, err := os.Open(s.path); s.f = f; return f, err }
func (s *sessionFile) Close() error {
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}

const sessionQuietPeriod = 15 * time.Minute

type sessionMarker struct {
	StartedAt time.Time `json:"started_at"`
}

// DiscoverSessions walks logsDir/<ticket>/pi-sessions/<stage>/*.jsonl and
// returns readers for each discovered file. A recent file or the file named by
// a live <stage>.session marker is flagged for exclusion as live_resume.
func DiscoverSessions(logsDir, stage string) ([]SessionReader, error) {
	return discoverSessions(logsDir, stage, time.Now())
}

func discoverSessions(logsDir, stage string, now time.Time) ([]SessionReader, error) {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return nil, fmt.Errorf("read logs dir: %w", err)
	}

	var sessions []SessionReader
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		ticket := entry.Name()
		stageDir := filepath.Join(logsDir, ticket, "pi-sessions", stage)
		files, err := filepath.Glob(filepath.Join(stageDir, "*.jsonl"))
		if err != nil || len(files) == 0 {
			continue
		}
		sort.Strings(files)

		livePaths := make(map[string]bool)
		markerPath := filepath.Join(logsDir, ticket, stage+".session")
		markerData, markerErr := os.ReadFile(markerPath)
		switch {
		case markerErr == nil:
			var marker sessionMarker
			if json.Unmarshal(markerData, &marker) != nil || marker.StartedAt.IsZero() {
				for _, file := range files {
					livePaths[file] = true
				}
			} else if file := newestSessionFileSince(files, marker.StartedAt); file != "" {
				livePaths[file] = true
			}
		case !os.IsNotExist(markerErr):
			for _, file := range files {
				livePaths[file] = true
			}
		}

		freshAfter := now.Add(-sessionQuietPeriod)
		for _, file := range files {
			lastActivity, timestamped := latestSessionActivity(file)
			fresh := timestamped && lastActivity.After(freshAfter)
			if !timestamped {
				info, statErr := os.Stat(file)
				fresh = statErr != nil || info.ModTime().After(freshAfter)
			}
			sessions = append(sessions, &sessionFile{
				path:   file,
				ticket: ticket,
				stage:  stage,
				live:   livePaths[file] || fresh,
			})
		}
	}

	return sessions, nil
}

var timestampPattern = regexp.MustCompile(`"timestamp":"([^"]+)"`)

func latestSessionActivity(path string) (time.Time, bool) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineStart := true
	var latest time.Time
	for {
		part, isPrefix, readErr := reader.ReadLine()
		if lineStart {
			if match := timestampPattern.FindSubmatch(part); len(match) == 2 {
				if timestamp, parseErr := time.Parse(time.RFC3339Nano, string(match[1])); parseErr == nil && timestamp.After(latest) {
					latest = timestamp
				}
			}
		}
		lineStart = !isPrefix
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return latest, !latest.IsZero()
		}
	}
	return latest, !latest.IsZero()
}

func newestSessionFileSince(files []string, startedAt time.Time) string {
	floor := startedAt.Add(-time.Second)
	var newest string
	var newestMod time.Time
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil || info.ModTime().Before(floor) {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest = file
			newestMod = info.ModTime()
		}
	}
	return newest
}

// --- Raw JSONL types --------------------------------------------------------

type rawEntry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ParentID   *string         `json:"parentId"`
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	Message    *rawMessage     `json:"message,omitempty"`

	// Compaction-specific fields.
	TokensBefore *int      `json:"tokensBefore,omitempty"`
	CompUsage    *rawUsage `json:"usage,omitempty"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage,omitempty"`

	// Tool result fields.
	ToolName string          `json:"toolName,omitempty"`
	Details  json.RawMessage `json:"details,omitempty"`
}

type rawUsage struct {
	Input      *int `json:"input"`
	Output     *int `json:"output"`
	CacheRead  *int `json:"cacheRead"`
	CacheWrite *int `json:"cacheWrite"`
}

type usageState uint8

const (
	usageMalformed usageState = iota
	usageEmpty
	usageValid
)

func (u *rawUsage) values() (input, output, cacheRead, cacheWrite int, state usageState) {
	if u == nil || u.Input == nil || u.Output == nil || u.CacheRead == nil || u.CacheWrite == nil {
		return 0, 0, 0, 0, usageMalformed
	}
	input, output, cacheRead, cacheWrite = *u.Input, *u.Output, *u.CacheRead, *u.CacheWrite
	if input < 0 || output < 0 || cacheRead < 0 || cacheWrite < 0 {
		return 0, 0, 0, 0, usageMalformed
	}
	if input == 0 && output == 0 && cacheRead == 0 && cacheWrite == 0 {
		return 0, 0, 0, 0, usageEmpty
	}
	if input+cacheRead+cacheWrite == 0 {
		return 0, 0, 0, 0, usageMalformed
	}
	return input, output, cacheRead, cacheWrite, usageValid
}

type rawTodoDetails struct {
	Operation string    `json:"operation"`
	Todos     []rawTodo `json:"todos"`
}

type rawTodo struct {
	ID     int    `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type rawCheckpointData struct {
	CompletedPhase string `json:"completedPhase"`
	NextPhase      string `json:"nextPhase"`
	Outcome        string `json:"outcome"`
}

// rawCheckpointDataSnake matches snake_case field names for compatibility.
type rawCheckpointDataSnake struct {
	CompletedPhase string `json:"completed_phase"`
	NextPhase      string `json:"next_phase"`
	Outcome        string `json:"outcome"`
}

// parseCheckpointData tries camelCase first, then falls back to snake_case.
func parseCheckpointData(data json.RawMessage) (rawCheckpointData, bool) {
	var cp rawCheckpointData
	if json.Unmarshal(data, &cp) != nil {
		return cp, false
	}
	if cp.CompletedPhase == "" {
		var alt rawCheckpointDataSnake
		if json.Unmarshal(data, &alt) == nil {
			if alt.CompletedPhase != "" {
				cp.CompletedPhase = alt.CompletedPhase
			}
			if alt.NextPhase != "" {
				cp.NextPhase = alt.NextPhase
			}
			if cp.Outcome == "" {
				cp.Outcome = alt.Outcome
			}
		}
	}
	return cp, cp.CompletedPhase != ""
}

// phasePattern matches todo titles starting with "Phase N" (case-insensitive).
var phasePattern = regexp.MustCompile(`(?i)^phase\s+(\d+)\b`)

// --- Session reader ---------------------------------------------------------

// ReadSession parses a pi JSONL session from r, reconstructs the active path
// by following parentId from the last leaf, and returns a SessionTrace.
//
// It uses bufio.Reader (not bufio.Scanner) to handle lines above the 1 MiB
// scanner cap that real sessions can contain.
func ReadSession(r io.Reader, file string) (*SessionTrace, error) {
	br := bufio.NewReader(r)

	var entries []rawEntry
	idIndex := make(map[string]int)
	duplicateID := false

	for {
		line, err := readLine(br)
		if len(line) > 0 {
			var e rawEntry
			if jerr := json.Unmarshal(line, &e); jerr != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				continue
			}
			idx := len(entries)
			entries = append(entries, e)
			if e.ID != "" {
				if _, exists := idIndex[e.ID]; exists {
					duplicateID = true
				}
				idIndex[e.ID] = idx
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read session %s: %w", file, err)
		}
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("read session %s: empty file", file)
	}

	trace := &SessionTrace{
		File:      file,
		SessionID: entries[0].ID,
	}
	if duplicateID {
		trace.Exclusion = ExcludeBrokenChain
		return trace, nil
	}

	// Check for branch_summary entries anywhere in the file.
	for _, e := range entries {
		if e.Type == "branch_summary" {
			trace.Exclusion = ExcludeBranchSummary
			return trace, nil
		}
	}

	// Build the active path.
	activePath, ok := buildActivePath(entries, idIndex)
	if !ok {
		trace.Exclusion = ExcludeBrokenChain
		return trace, nil
	}

	// Walk the active path and extract signals.
	extractSignals(trace, entries, activePath)

	return trace, nil
}

// extractSignals walks the active path once and populates the trace with
// assistant calls, checkpoints (both explicit and todo-based), and compaction
// records. It also sets the exclusion reason if appropriate.
func extractSignals(trace *SessionTrace, entries []rawEntry, activePath []int) {
	hasUsage := false
	hasMalformedUsage := false
	hasCompaction := false
	assistantCount := 0

	// Todo snapshot tracking for historical checkpoint detection.
	var todoSnaps []todoSnapshot
	explicitCheckpoints := make(map[string]int)

	for pathPos, entryIdx := range activePath {
		e := entries[entryIdx]

		switch e.Type {
		case "message":
			if e.Message == nil {
				continue
			}
			msg := e.Message

			if msg.Role == "assistant" {
				switch appendAssistantCall(trace, e.ID, msg.Usage) {
				case usageEmpty:
					continue
				case usageMalformed:
					hasMalformedUsage = true
					continue
				case usageValid:
					assistantCount++
					hasUsage = true
				}
			}

			// Collect todo write results for historical checkpoint detection.
			if msg.Role == "toolResult" && msg.ToolName == "manage_todo_list" && len(msg.Details) > 0 {
				var det rawTodoDetails
				if json.Unmarshal(msg.Details, &det) == nil && det.Operation == "write" {
					phases := extractPhases(det.Todos)
					if len(phases) > 0 {
						todoSnaps = append(todoSnaps, todoSnapshot{
							afterCallIdx: assistantCount - 1,
							phases:       phases,
						})
					}
				}
			}

		case "custom":
			if e.CustomType == "kontora-phase-checkpoint" && len(e.Data) > 0 {
				appendExplicitCheckpoint(trace, e.Data, assistantCount-1, explicitCheckpoints)
			}

		case "compaction":
			hasCompaction = true
			rec := Record{}
			if e.TokensBefore != nil {
				rec.TokensBefore = *e.TokensBefore
			}
			if input, output, _, _, state := e.CompUsage.values(); state == usageValid {
				rec.SummaryInput = input
				rec.SummaryOutput = output
			}

			rec.PostCompactionCtx, rec.HasPostCompaction = postCompactionContext(entries, activePath, pathPos+1)
			trace.Compactions = append(trace.Compactions, rec)
		}
	}

	// Detect historical checkpoints from todo snapshots when no explicit
	// kontora-phase-checkpoint entries exist.
	if len(trace.Checkpoints) == 0 && len(todoSnaps) > 1 {
		trace.Checkpoints = detectTodoCheckpoints(todoSnaps)
	}

	// Apply exclusions in priority order.
	if !hasUsage || hasMalformedUsage {
		trace.Exclusion = ExcludeNoUsage
	} else if hasCompaction {
		trace.Exclusion = ExcludeHasCompaction
	}
}

func postCompactionContext(entries []rawEntry, activePath []int, start int) (int, bool) {
	for _, entryIdx := range activePath[start:] {
		next := entries[entryIdx]
		if next.Type != "message" || next.Message == nil || next.Message.Role != "assistant" {
			continue
		}
		input, _, cacheRead, cacheWrite, state := next.Message.Usage.values()
		if state == usageEmpty {
			continue
		}
		if state == usageValid {
			return input + cacheRead + cacheWrite, true
		}
		return 0, false
	}
	return 0, false
}

func appendAssistantCall(trace *SessionTrace, id string, usage *rawUsage) usageState {
	input, output, cacheRead, cacheWrite, state := usage.values()
	if state != usageValid {
		return state
	}
	ctx := input + cacheRead + cacheWrite
	trace.AssistantCalls = append(trace.AssistantCalls, AssistantCall{
		ID:            id,
		PromptContext: ctx,
		Output:        output,
	})
	trace.TotalPrompt += ctx
	trace.TotalOutput += output
	return usageValid
}

func appendExplicitCheckpoint(trace *SessionTrace, data json.RawMessage, afterCallIndex int, seen map[string]int) {
	cp, ok := parseCheckpointData(data)
	if !ok {
		return
	}
	if checkpointIdx, exists := seen[cp.CompletedPhase]; exists {
		if cp.NextPhase != "" {
			trace.Checkpoints[checkpointIdx].NextPhase = cp.NextPhase
		}
		if cp.Outcome != "" {
			trace.Checkpoints[checkpointIdx].Outcome = cp.Outcome
		}
		return
	}
	seen[cp.CompletedPhase] = len(trace.Checkpoints)
	trace.Checkpoints = append(trace.Checkpoints, Checkpoint{
		AfterCallIndex: afterCallIndex,
		CompletedPhase: cp.CompletedPhase,
		NextPhase:      cp.NextPhase,
		Source:         "explicit",
		Outcome:        cp.Outcome,
	})
}

// readLine reads one complete line from a bufio.Reader, handling lines that
// exceed the internal buffer size. Returns the line without trailing newline.
func readLine(br *bufio.Reader) ([]byte, error) {
	var result []byte
	for {
		part, isPrefix, err := br.ReadLine()
		result = append(result, part...)
		if !isPrefix || err != nil {
			return result, err
		}
	}
}

// buildActivePath finds the last leaf entry and walks parentId back to the
// root. Returns entry indices in chronological (root-first) order.
func buildActivePath(entries []rawEntry, idIndex map[string]int) ([]int, bool) {
	if len(entries) == 0 {
		return nil, false
	}

	// Find IDs that are referenced as parentId by at least one entry.
	isParent := make(map[string]bool)
	for _, e := range entries {
		if e.ParentID != nil && *e.ParentID != "" {
			isParent[*e.ParentID] = true
		}
	}

	// Find the last leaf: the last entry whose ID is not a parent of any other.
	leafIdx := -1
	for i, entry := range slices.Backward(entries) {
		if entry.ID != "" && !isParent[entry.ID] {
			leafIdx = i
			break
		}
	}
	if leafIdx == -1 {
		leafIdx = len(entries) - 1
	}

	// Walk backwards from the leaf.
	var path []int
	visited := make(map[int]bool)
	cur := leafIdx

	for {
		if visited[cur] {
			return nil, false
		}
		visited[cur] = true
		path = append(path, cur)

		e := entries[cur]
		if e.ParentID == nil || *e.ParentID == "" {
			break
		}
		parentIdx, ok := idIndex[*e.ParentID]
		if !ok || parentIdx >= cur {
			return nil, false
		}
		cur = parentIdx
	}

	// Reverse to chronological order.
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path, true
}

// extractPhases extracts phase numbers and statuses from todo items.
func extractPhases(todos []rawTodo) map[int]string {
	phases := make(map[int]string)
	for _, todo := range todos {
		m := phasePattern.FindStringSubmatch(todo.Title)
		if m == nil {
			continue
		}
		num, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		status, exists := phases[num]
		if !exists || strings.EqualFold(status, "completed") {
			phases[num] = todo.Status
		}
	}
	return phases
}

// todoSnapshot is one manage_todo_list write result with phase statuses.
type todoSnapshot struct {
	afterCallIdx int
	phases       map[int]string
}

// detectTodoCheckpoints derives checkpoints from successive todo write result
// snapshots. A checkpoint is detected when a phase group transitions from
// not-completed to completed, with at least one later phase group remaining
// unfinished.
func detectTodoCheckpoints(snaps []todoSnapshot) []Checkpoint {
	var checkpoints []Checkpoint
	seen := make(map[int]bool) // phase numbers already emitted
	prevCompleted := make(map[int]bool)

	for _, snap := range snaps {
		for num, status := range snap.phases {
			nowDone := strings.EqualFold(status, "completed")
			if nowDone && !prevCompleted[num] && !seen[num] {
				// Require at least one later phase to be unfinished.
				hasLater := false
				for other, otherStatus := range snap.phases {
					if other > num && !strings.EqualFold(otherStatus, "completed") {
						hasLater = true
						break
					}
				}
				if hasLater {
					seen[num] = true
					nextNum := nextPhaseNum(snap.phases, num)
					checkpoints = append(checkpoints, Checkpoint{
						AfterCallIndex: snap.afterCallIdx,
						CompletedPhase: fmt.Sprintf("Phase %d", num),
						NextPhase:      fmt.Sprintf("Phase %d", nextNum),
						Source:         "todo",
					})
				}
			}
		}
		// Update previous state.
		for num, status := range snap.phases {
			prevCompleted[num] = strings.EqualFold(status, "completed")
		}
	}

	return checkpoints
}

func nextPhaseNum(phases map[int]string, after int) int {
	var nums []int
	for num := range phases {
		if num > after {
			nums = append(nums, num)
		}
	}
	if len(nums) == 0 {
		return 0
	}
	sort.Ints(nums)
	return nums[0]
}

// --- Calibration ------------------------------------------------------------

// BuildCalibration extracts calibration statistics from compaction records
// across all traces.
func BuildCalibration(traces []*SessionTrace) Calibration {
	var cal Calibration
	for _, t := range traces {
		for _, c := range t.Compactions {
			if c.TokensBefore <= 0 || c.SummaryInput <= 0 || !c.HasPostCompaction || c.PostCompactionCtx <= 0 {
				continue
			}
			cal.SampleCount++
			cal.SummaryRatios = append(cal.SummaryRatios, float64(c.SummaryInput)/float64(c.TokensBefore))
			cal.SummaryOutputs = append(cal.SummaryOutputs, c.SummaryOutput)
			cal.PostCompactionCtx = append(cal.PostCompactionCtx, c.PostCompactionCtx)
		}
	}
	sort.Float64s(cal.SummaryRatios)
	sort.Ints(cal.SummaryOutputs)
	sort.Ints(cal.PostCompactionCtx)
	return cal
}

// DeriveScenarios returns low, median, and high calibration scenarios. Returns
// nil if there are no calibration samples.
func DeriveScenarios(cal Calibration) []CalibrationScenario {
	if cal.SampleCount == 0 || len(cal.PostCompactionCtx) == 0 {
		return nil
	}

	pcts := [3]struct {
		label string
		p     float64
	}{
		{"low", 0.25},
		{"median", 0.5},
		{"high", 0.75},
	}

	scenarios := make([]CalibrationScenario, 3)
	for i, p := range pcts {
		s := CalibrationScenario{Label: p.label}
		s.SummaryRatio = PercentileFloat64(cal.SummaryRatios, p.p)
		s.SummaryOutput = PercentileInt(cal.SummaryOutputs, p.p)
		s.PostCompactionCtx = PercentileInt(cal.PostCompactionCtx, p.p)
		scenarios[i] = s
	}
	return scenarios
}

// --- Threshold replay -------------------------------------------------------

// ProjectThreshold replays all eligible traces against one threshold and
// calibration scenario and returns the aggregate result.
func ProjectThreshold(traces []*SessionTrace, threshold int, scenario CalibrationScenario) ScenarioResult {
	var agg ScenarioResult
	agg.Label = scenario.Label

	for _, t := range traces {
		if t.Exclusion != "" {
			continue
		}
		sr := replaySession(t, threshold, scenario)
		agg.CompactionCount += sr.CompactionCount
		agg.ProjectedPrompt += sr.ProjectedPrompt
		agg.ProjectedOutput += sr.ProjectedOutput
		agg.ProjectedSummary += sr.ProjectedSummary
		agg.BaselinePrompt += sr.BaselinePrompt
		agg.BaselineOutput += sr.BaselineOutput
	}

	agg.PromptReduction = agg.BaselinePrompt - agg.ProjectedPrompt
	agg.RawTokenReduction = agg.PromptReduction - agg.ProjectedSummary
	return agg
}

// replaySession replays one session's assistant calls against a threshold and
// calibration scenario, compacting at checkpoints where projected context
// exceeds the threshold.
func replaySession(t *SessionTrace, threshold int, scenario CalibrationScenario) ScenarioResult {
	var res ScenarioResult
	if len(t.AssistantCalls) == 0 {
		return res
	}

	// Build a set of assistant call indices that are checkpoint boundaries.
	cpAfter := make(map[int]bool)
	for _, cp := range t.Checkpoints {
		if cp.AfterCallIndex >= 0 {
			cpAfter[cp.AfterCallIndex] = true
		}
	}

	// Replay: baseOffset adjusts observed prompt context to model what it
	// would be after a hypothetical compaction.
	baseOffset := 0

	for i, call := range t.AssistantCalls {
		res.BaselinePrompt += call.PromptContext
		res.BaselineOutput += call.Output

		projectedCtx := call.PromptContext + baseOffset
		projectedCtx = max(projectedCtx, 0)
		res.ProjectedPrompt += projectedCtx
		res.ProjectedOutput += call.Output

		// Evaluate compaction at checkpoint.
		if cpAfter[i] && projectedCtx > threshold {
			res.CompactionCount++

			summaryInput := int(float64(projectedCtx) * scenario.SummaryRatio)
			summaryOutput := scenario.SummaryOutput
			res.ProjectedSummary += summaryInput + summaryOutput

			// After compaction, the next call's context resets.
			if i+1 < len(t.AssistantCalls) {
				nextObserved := t.AssistantCalls[i+1].PromptContext
				if scenario.PostCompactionCtx > 0 {
					baseOffset = scenario.PostCompactionCtx - nextObserved
				} else {
					baseOffset = summaryInput - nextObserved
				}
			}
		}
	}

	res.PromptReduction = res.BaselinePrompt - res.ProjectedPrompt
	res.RawTokenReduction = res.PromptReduction - res.ProjectedSummary
	return res
}

// --- High-level entry point -------------------------------------------------

// Estimate runs the full estimation pipeline over a set of session readers.
// The top parameter controls how many top sessions to include, ranked by
// projected median raw-token reduction at the lowest configured threshold.
func Estimate(sessions []SessionReader, thresholds []int, top int) (*Report, error) {
	report := &Report{
		Excluded: make(map[ExclusionReason]int),
	}

	var traces []*SessionTrace
	for _, sr := range sessions {
		report.FilesScanned++
		r, err := sr.Open()
		if err != nil {
			report.Excluded[ExcludeUnreadable]++
			continue
		}
		t, err := ReadSession(r, sr.Path())
		_ = sr.Close()
		if err != nil {
			report.Excluded[ExcludeUnreadable]++
			continue
		}

		// Apply metadata from discovery when available.
		if info, ok := sr.(SessionInfo); ok {
			t.Ticket = info.Ticket()
			t.Stage = info.Stage()
			if info.IsLive() && t.Exclusion == "" {
				t.Exclusion = ExcludeLiveResume
			}
		}

		traces = append(traces, t)
		if t.Exclusion != "" {
			report.Excluded[t.Exclusion]++
		} else {
			report.Eligible++
			report.Checkpoints += len(t.Checkpoints)
		}
	}

	cal := BuildCalibration(traces)
	report.Calibration = cal

	scenarios := DeriveScenarios(cal)
	if scenarios == nil {
		return report, nil
	}

	sort.Ints(thresholds)
	for _, th := range thresholds {
		tr := ThresholdResult{Threshold: th}
		for i, s := range scenarios {
			if i >= 3 {
				break
			}
			tr.Scenarios[i] = ProjectThreshold(traces, th, s)
		}
		report.Thresholds = append(report.Thresholds, tr)
	}

	// Compute top sessions by projected median raw-token reduction at the
	// lowest configured threshold.
	if top > 0 && len(thresholds) > 0 && len(scenarios) >= 2 {
		lowestThreshold := thresholds[0]
		medianScenario := scenarios[1]

		var topResults []TopSessionResult
		for _, t := range traces {
			if t.Exclusion != "" || len(t.Checkpoints) == 0 {
				continue
			}
			sr := replaySession(t, lowestThreshold, medianScenario)
			if sr.RawTokenReduction <= 0 {
				continue
			}
			topResults = append(topResults, TopSessionResult{
				Ticket:          t.Ticket,
				Stage:           t.Stage,
				File:            t.File,
				BaselineTokens:  sr.BaselinePrompt + sr.BaselineOutput,
				Checkpoints:     len(t.Checkpoints),
				MedianReduction: sr.RawTokenReduction,
			})
		}

		sort.Slice(topResults, func(i, j int) bool {
			return topResults[i].MedianReduction > topResults[j].MedianReduction
		})

		if len(topResults) > top {
			topResults = topResults[:top]
		}
		report.TopSessions = topResults
	}

	return report, nil
}

// --- Percentile helpers -----------------------------------------------------

// PercentileFloat64 returns the p-th percentile of a sorted float64 slice
// using linear interpolation.
func PercentileFloat64(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi || hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// PercentileInt returns the p-th percentile of a sorted int slice using
// linear interpolation.
func PercentileInt(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi || hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return int(math.Round(float64(sorted[lo])*(1-frac) + float64(sorted[hi])*frac))
}
