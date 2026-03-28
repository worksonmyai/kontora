package pipeline

import (
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type EventKind int

const (
	EventPickedUp EventKind = iota // daemon found status=todo
	EventAgentExited
)

type Event struct {
	Kind      EventKind
	ExitCode  int
	Timestamp time.Time
}

type ActionKind int

const (
	ActionSpawn ActionKind = iota
	ActionAdvance
	ActionComplete
	ActionRetry
	ActionBack
	ActionPause // needs human attention
	ActionPark  // park in custom status
)

func (k ActionKind) String() string {
	switch k {
	case ActionSpawn:
		return "spawn"
	case ActionAdvance:
		return "advance"
	case ActionComplete:
		return "complete"
	case ActionRetry:
		return "retry"
	case ActionBack:
		return "back"
	case ActionPause:
		return "pause"
	case ActionPark:
		return "park"
	default:
		return fmt.Sprintf("ActionKind(%d)", int(k))
	}
}

type FieldUpdate struct {
	Key   string
	Value any
}

type SpawnInfo struct {
	Agent string
	Stage string
}

type Action struct {
	Kind    ActionKind
	Fields  []FieldUpdate
	History *ticket.HistoryEntry
	Spawn   *SpawnInfo
}

func Evaluate(t *ticket.Ticket, pipeline config.Pipeline, ev Event) (Action, error) {
	switch ev.Kind {
	case EventPickedUp:
		return handlePickup(t, pipeline, ev)
	case EventAgentExited:
		return handleAgentExited(t, pipeline, ev)
	default:
		return Action{}, fmt.Errorf("unknown event kind: %d", ev.Kind)
	}
}

func handlePickup(t *ticket.Ticket, pipeline config.Pipeline, ev Event) (Action, error) {
	if t.Status != ticket.StatusTodo {
		return Action{}, fmt.Errorf("pickup requires status=todo, got %q", t.Status)
	}

	idx, err := stageIndex(pipeline, t.Stage)
	if err != nil {
		return Action{}, err
	}

	step := pipeline[idx]

	return Action{
		Kind: ActionSpawn,
		Fields: []FieldUpdate{
			{Key: "status", Value: string(ticket.StatusInProgress)},
			{Key: "started_at", Value: ev.Timestamp},
		},
		Spawn: &SpawnInfo{
			Agent: step.Agent,
			Stage: step.Stage,
		},
	}, nil
}

func handleAgentExited(t *ticket.Ticket, pipeline config.Pipeline, ev Event) (Action, error) {
	if t.Status != ticket.StatusInProgress {
		return Action{}, fmt.Errorf("agent_exited requires status=in_progress, got %q", t.Status)
	}

	idx, err := stageIndex(pipeline, t.Stage)
	if err != nil {
		return Action{}, err
	}

	step := pipeline[idx]
	history := &ticket.HistoryEntry{
		Stage:       step.Stage,
		Agent:       step.Agent,
		ExitCode:    ev.ExitCode,
		StartedAt:   t.StartedAt,
		CompletedAt: &ev.Timestamp,
	}

	if ev.ExitCode == 0 {
		return handleSuccess(step, idx, pipeline, ev, history)
	}
	return handleFailure(step, idx, pipeline, t, history)
}

func handleSuccess(step config.PipelineStep, idx int, pipeline config.Pipeline, ev Event, history *ticket.HistoryEntry) (Action, error) {
	switch step.OnSuccess {
	case "done":
		return Action{
			Kind: ActionComplete,
			Fields: []FieldUpdate{
				{Key: "status", Value: string(ticket.StatusDone)},
				{Key: "completed_at", Value: ev.Timestamp},
			},
			History: history,
		}, nil

	case "next":
		if idx+1 >= len(pipeline) {
			return Action{}, fmt.Errorf("on_success=next on last stage %q", step.Stage)
		}
		next := pipeline[idx+1]
		return Action{
			Kind: ActionAdvance,
			Fields: []FieldUpdate{
				{Key: "status", Value: string(ticket.StatusTodo)},
				{Key: "stage", Value: next.Stage},
				{Key: "attempt", Value: 0},
			},
			History: history,
		}, nil

	default:
		return Action{
			Kind: ActionPark,
			Fields: []FieldUpdate{
				{Key: "status", Value: step.OnSuccess},
			},
			History: history,
		}, nil
	}
}

func handleFailure(step config.PipelineStep, idx int, pipeline config.Pipeline, t *ticket.Ticket, history *ticket.HistoryEntry) (Action, error) {
	switch step.OnFailure {
	case "retry":
		if t.Attempt < step.MaxRetries {
			return Action{
				Kind: ActionRetry,
				Fields: []FieldUpdate{
					{Key: "status", Value: string(ticket.StatusTodo)},
					{Key: "attempt", Value: t.Attempt + 1},
				},
				History: history,
			}, nil
		}
		// Retries exhausted → pause
		return Action{
			Kind: ActionPause,
			Fields: []FieldUpdate{
				{Key: "status", Value: string(ticket.StatusPaused)},
			},
			History: history,
		}, nil

	case "back":
		if idx == 0 {
			return Action{}, fmt.Errorf("cannot go back from first stage")
		}
		prev := pipeline[idx-1]
		return Action{
			Kind: ActionBack,
			Fields: []FieldUpdate{
				{Key: "status", Value: string(ticket.StatusTodo)},
				{Key: "stage", Value: prev.Stage},
				{Key: "attempt", Value: 0},
			},
			History: history,
		}, nil

	case "pause":
		return Action{
			Kind: ActionPause,
			Fields: []FieldUpdate{
				{Key: "status", Value: string(ticket.StatusPaused)},
			},
			History: history,
		}, nil

	default:
		return Action{
			Kind: ActionPark,
			Fields: []FieldUpdate{
				{Key: "status", Value: step.OnFailure},
			},
			History: history,
		}, nil
	}
}

func stageIndex(pipeline config.Pipeline, stage string) (int, error) {
	for i, step := range pipeline {
		if step.Stage == stage {
			return i, nil
		}
	}
	return -1, fmt.Errorf("stage %q not found in pipeline", stage)
}
