package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The artifacts one run of a stage leaves behind.
const (
	// ArtifactSession is the agent's own session JSONL.
	ArtifactSession = "session"
	// ArtifactLog is Kontora's plaintext capture of the agent's output. It is
	// per stage, not per run: every run of a stage overwrites it.
	ArtifactLog = "log"
	// ArtifactEvents is Kontora's parsed activity sidecar, written per run.
	ArtifactEvents = "events"
)

// UnknownRun is the run number of a file no history row claims.
const UnknownRun = -1

// Layout is where one ticket's files live on this machine. Every directory is
// already expanded: the package resolves paths, it does not interpret `~`.
type Layout struct {
	LogsDir         string
	TicketID        string
	ClaudeConfigDir string
}

// Ref is a session reference read from a history row.
type Ref struct {
	Kind string
	Ref  string
}

// Run is one history row, reduced to what locating its files needs.
type Run struct {
	Stage string
	// Index is the run's zero-based position among the runs of its stage.
	Index int
	// Agent is the agent's configured name, which is all a row written by an
	// agent Kontora cannot locate a session for has to say for itself.
	Agent string
	// Kind is that agent's runtime as the config reads today, or "" when the
	// config no longer knows the agent. It is only consulted for a row written
	// before Ref existed.
	Kind string
	Ref  Ref
}

// File is one artifact of one run. Exactly one of Path and Note carries the
// answer: a resolved file has a Path, and everything else has a Note saying
// why there is none.
type File struct {
	Stage    string
	Run      int
	Artifact string
	Path     string
	Note     string
}

// Artifacts selects which files to emit. All false means sessions only.
type Artifacts struct {
	Sessions bool
	Logs     bool
	Events   bool
}

// noSessionRecorded is what a row written before the reference fields existed
// can say. The run happened; which session it wrote is not on disk anywhere.
const noSessionRecorded = "no session recorded (run predates session_ref)"

// Resolve returns the absolute path the reference names, or the reason it
// names nothing on this machine. Exactly one of the two is non-empty.
func (r Ref) Resolve(l Layout) (path, reason string) {
	if !SafeRef(r.Kind, r.Ref) {
		return "", fmt.Sprintf("session reference refused: %q", r.Ref)
	}
	switch r.Kind {
	case KindClaude:
		matches, pattern, err := ClaudeFiles(l.ClaudeConfigDir, r.Ref)
		if err != nil {
			return "", fmt.Sprintf("session file not found: %v", err)
		}
		if len(matches) == 0 {
			return "", fmt.Sprintf("session file missing: no %s", pattern)
		}
		return matches[0], ""
	case KindPi:
		p := filepath.Join(TicketDir(l.LogsDir, l.TicketID), filepath.FromSlash(r.Ref))
		if !exists(p) {
			return "", "session file missing: " + p
		}
		return p, ""
	}
	return "", fmt.Sprintf("unknown session runtime %q", r.Kind)
}

// StageRecord returns the session record a Claude stage left behind: the one
// its last run completed with, or failing that the one a run still in flight
// planted. It names exactly one session, so it can only ever answer for a
// stage's newest run.
func StageRecord(l Layout, stage string) *Record {
	if !SafeStage(stage) {
		return nil
	}
	for _, p := range []string{
		CompletedRecordPath(l.LogsDir, l.TicketID, stage),
		RecordPath(l.LogsDir, l.TicketID, stage),
	} {
		if rec, err := ReadRecord(p); err == nil && rec.SessionID != "" {
			return rec
		}
	}
	return nil
}

// RunFiles returns the per-run artifacts of one history row: its session JSONL
// and its activity sidecar. The stage log is not among them because it is per
// stage; Runs emits it once.
//
// lastOfStage says whether this row is the newest run of its stage, which is
// the only row a stage's session record can be attributed to.
func RunFiles(l Layout, a Artifacts, r Run, lastOfStage bool) []File {
	var out []File
	if a.Sessions {
		out = append(out, sessionFile(l, r, lastOfStage))
	}
	if a.Events {
		f := File{Stage: r.Stage, Run: r.Index, Artifact: ArtifactEvents}
		switch {
		case !SafeStage(r.Stage):
			f.Note = fmt.Sprintf("stage name refused: %q", r.Stage)
		default:
			p := EventsPath(l.LogsDir, l.TicketID, r.Stage, r.Index)
			if exists(p) {
				f.Path = p
			} else {
				f.Note = "activity sidecar missing: " + p
			}
		}
		out = append(out, f)
	}
	return out
}

func sessionFile(l Layout, r Run, lastOfStage bool) File {
	f := File{Stage: r.Stage, Run: r.Index, Artifact: ArtifactSession}
	switch {
	case r.Ref.Ref != "":
		f.Path, f.Note = r.Ref.Resolve(l)
	case r.Kind == KindPi:
		// The stage's session directory holds the files, but nothing says which
		// run wrote which: a resumed run appends to an existing file and a run
		// that died before pi's first reply wrote none, so neither the count nor
		// the modification order is the run order. Runs lists them unattributed.
		f.Note = noSessionRecorded
	case r.Kind == KindClaude:
		rec := StageRecord(l, r.Stage)
		if !lastOfStage || rec == nil {
			f.Note = noSessionRecorded
			break
		}
		f.Path, f.Note = Ref{Kind: KindClaude, Ref: rec.SessionID}.Resolve(l)
		if f.Note == "" {
			f.Note = "recovered from the stage's session record"
		}
	case r.Agent != "":
		f.Note = fmt.Sprintf("agent %q writes no session", r.Agent)
	default:
		f.Note = noSessionRecorded
	}
	return f
}

// Runs returns the files behind a ticket's runs: one row per history row, in
// history order, followed by the files no row claims.
//
// No history row is ever dropped. A row whose session cannot be found is
// reported with the reason instead, because omitting it reads as "this stage
// never ran".
func Runs(l Layout, a Artifacts, runs []Run) []File {
	lastOfStage := make(map[string]int, len(runs))
	for i, r := range runs {
		lastOfStage[r.Stage] = i
	}

	var (
		out           []File
		stages        []string
		seenStage     = make(map[string]bool, len(runs))
		claimedPath   = make(map[string]File, len(runs))
		claimedClaude = make(map[string]bool, len(runs))
	)

	for i, r := range runs {
		if !seenStage[r.Stage] {
			seenStage[r.Stage] = true
			stages = append(stages, r.Stage)
			if a.Logs {
				out = append(out, logFile(l, r.Stage))
			}
		}
		last := lastOfStage[r.Stage] == i
		if r.Ref.Kind == KindClaude && r.Ref.Ref != "" {
			claimedClaude[r.Ref.Ref] = true
		} else if last && r.Ref.Ref == "" && r.Kind == KindClaude {
			if rec := StageRecord(l, r.Stage); rec != nil {
				claimedClaude[rec.SessionID] = true
			}
		}

		for _, f := range RunFiles(l, a, r, last) {
			if f.Artifact == ArtifactSession && f.Path != "" {
				if prior, ok := claimedPath[f.Path]; ok {
					f.Note = sharedFileNote(prior, f)
				} else {
					claimedPath[f.Path] = f
				}
			}
			out = append(out, f)
		}
	}

	if a.Sessions {
		out = append(out, unclaimed(l, stages, claimedPath, claimedClaude)...)
	}
	return out
}

// sharedFileNote labels a run whose session file another run already wrote to.
// A resumed run continues the earlier conversation and appends to its file, so
// one file can back several runs and the output must not imply otherwise.
func sharedFileNote(prior, f File) string {
	if prior.Stage == f.Stage {
		return fmt.Sprintf("resumed: same file as run %d", prior.Run)
	}
	return fmt.Sprintf("resumed: same file as %s run %d", prior.Stage, prior.Run)
}

// unclaimed reports the session files sitting under the ticket that no history
// row accounts for: pi transcripts from before the reference fields existed,
// and the record of a run still in flight.
func unclaimed(l Layout, stages []string, claimedPath map[string]File, claimedClaude map[string]bool) []File {
	var out []File
	for _, stage := range sweepStages(l, stages) {
		// An annotation run that opened its own session writes beside the
		// stage's directory rather than into it, so both are listed.
		for _, dir := range []string{stage, stage + "-annotation"} {
			files, err := PiFiles(PiDir(l.LogsDir, l.TicketID, dir))
			if err != nil {
				continue
			}
			for _, p := range files {
				if _, ok := claimedPath[p]; ok {
					continue
				}
				out = append(out, File{
					Stage:    dir,
					Run:      UnknownRun,
					Artifact: ArtifactSession,
					Path:     p,
					Note:     fmt.Sprintf("discovered in %s/%s; not attributable to a run", PiSessionsDir, dir),
				})
			}
		}

		rec, err := ReadRecord(RecordPath(l.LogsDir, l.TicketID, stage))
		if err != nil || rec.SessionID == "" || claimedClaude[rec.SessionID] {
			continue
		}
		f := File{Stage: stage, Run: UnknownRun, Artifact: ArtifactSession}
		f.Path, f.Note = Ref{Kind: KindClaude, Ref: rec.SessionID}.Resolve(l)
		if f.Note == "" {
			f.Note = "running now, or interrupted"
		} else {
			f.Note += "; running now, or interrupted"
		}
		out = append(out, f)
	}
	return out
}

// sweepStages is the stages to look for unclaimed files under: the ones history
// names, in history order, then any the ticket's log directory holds a record
// or a session directory for, in name order.
//
// The stage running right now is the reason for the second half. It has no
// history row until it exits, and it is the transcript a reader most often
// wants.
func sweepStages(l Layout, fromHistory []string) []string {
	var (
		out  []string
		seen = make(map[string]bool, len(fromHistory))
	)
	add := func(stage string) {
		if !SafeStage(stage) || seen[stage] {
			return
		}
		seen[stage] = true
		out = append(out, stage)
	}
	for _, stage := range fromHistory {
		add(stage)
	}

	dir := TicketDir(l.LogsDir, l.TicketID)
	var found []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		for _, suffix := range []string{".session", ".completed-session"} {
			if name, ok := strings.CutSuffix(e.Name(), suffix); ok {
				found = append(found, name)
				break
			}
		}
	}
	piEntries, _ := os.ReadDir(filepath.Join(dir, PiSessionsDir))
	for _, e := range piEntries {
		if e.IsDir() {
			// The stage's own directory is what the loop scans; it covers the
			// annotation one from the base name.
			found = append(found, strings.TrimSuffix(e.Name(), "-annotation"))
		}
	}
	sort.Strings(found)
	for _, stage := range found {
		add(stage)
	}
	return out
}

func logFile(l Layout, stage string) File {
	f := File{Stage: stage, Run: UnknownRun, Artifact: ArtifactLog}
	if !SafeStage(stage) {
		f.Note = fmt.Sprintf("stage name refused: %q", stage)
		return f
	}
	p := LogPath(l.LogsDir, l.TicketID, stage)
	if exists(p) {
		f.Path = p
		return f
	}
	f.Note = "stage log missing: " + p
	return f
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
