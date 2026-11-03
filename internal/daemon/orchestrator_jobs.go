package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/worksonmyai/kontora/internal/orchestrator"
	controlstore "github.com/worksonmyai/kontora/internal/orchestrator/store"
	"github.com/worksonmyai/kontora/internal/process"
	"github.com/worksonmyai/kontora/internal/session"
)

type managedExecution struct {
	store    *controlstore.Store
	runner   RunnerFunc
	logsDir  string
	capacity int
}

func (e *managedExecution) reserve(attempt orchestrator.Attempt, leases []orchestrator.Lease) error {
	return transactControl(e.store, func(state orchestrator.State) (orchestrator.State, error) {
		return orchestrator.Apply(state, orchestrator.Command{Kind: orchestrator.AdmitAttempt, ExpectedRevision: state.Revision, Attempt: attempt, Leases: leases, Capacity: e.capacity, At: time.Now()})
	})
}

func (e *managedExecution) transition(workID, attemptID string, kind orchestrator.CommandKind, reason string) error {
	return transactControl(e.store, func(state orchestrator.State) (orchestrator.State, error) {
		return orchestrator.Apply(state, orchestrator.Command{Kind: kind, ExpectedRevision: state.Revision, WorkID: workID, AttemptID: attemptID, Reason: reason})
	})
}

// Retry revision conflicts without retrying a rejected transition or a failed durable write.
func transactControl(store *controlstore.Store, change func(orchestrator.State) (orchestrator.State, error)) error {
	for {
		state, err := store.State()
		if err != nil {
			return err
		}
		matched := false
		err = store.Transact(state.Revision, func(current orchestrator.State) (orchestrator.State, error) {
			matched = true
			return change(current)
		})
		if !matched && errors.Is(err, orchestrator.ErrStale) {
			continue
		}
		return err
	}
}

type frozenCommand struct {
	Binary      string            `json:"binary"`
	Args        []string          `json:"args"`
	Directory   string            `json:"directory"`
	Environment map[string]string `json:"environment"`
}

func (e *managedExecution) freeze(profile string, timeout time.Duration, command frozenCommand) (orchestrator.Recipe, error) {
	if profile == "" || timeout <= 0 || command.Binary == "" || command.Directory == "" {
		return orchestrator.Recipe{}, errors.New("managed execution requires a complete recipe")
	}
	data, err := json.Marshal(command)
	if err != nil {
		return orchestrator.Recipe{}, err
	}
	artifact, err := e.store.PutArtifact(data)
	if err != nil {
		return orchestrator.Recipe{}, err
	}
	return orchestrator.Recipe{Profile: profile, Timeout: timeout, ConfigurationRevision: artifact.Hash, Artifacts: []orchestrator.ArtifactRef{artifact}}, nil
}

func (e *managedExecution) execute(ctx context.Context, attemptID string) bool {
	state, err := e.store.State()
	if err != nil {
		return false
	}
	attempt, ok := state.Attempts[attemptID]
	if !ok || !attempt.Reservation.Held || attempt.Status != orchestrator.AttemptRunning || state.Work[attempt.WorkID].ActiveAttemptID != attemptID {
		return false
	}
	if len(attempt.Recipe.Artifacts) != 1 || attempt.Recipe.Artifacts[0].Hash != attempt.Recipe.ConfigurationRevision || attempt.Recipe.Timeout <= 0 {
		return false
	}
	data, err := e.store.ReadArtifact(attempt.Recipe.Artifacts[0])
	if err != nil {
		return false
	}
	var command frozenCommand
	if err := json.Unmarshal(data, &command); err != nil {
		return false
	}
	params := RunnerParams{Binary: command.Binary, Args: command.Args, Dir: command.Directory, Env: command.Environment, Timeout: attempt.Recipe.Timeout, ReplaceEnv: true, RequireTermination: true, AttemptID: attemptID}
	params.LogFile, err = session.AttemptLogPath(e.logsDir, attemptID)
	if err != nil {
		return false
	}
	result, runErr := e.runner(ctx, params)
	if result.Termination != process.TerminationProcessGroupGone && result.Termination != process.TerminationNotStarted {
		reason := "runner did not confirm process group termination"
		if runErr != nil {
			reason = runErr.Error()
		}
		_ = e.transition(attempt.WorkID, attemptID, orchestrator.UncertainAttempt, reason)
		return false
	}
	err = transactControl(e.store, func(current orchestrator.State) (orchestrator.State, error) {
		a, ok := current.Attempts[attemptID]
		if !ok || a.WorkID != attempt.WorkID || current.Work[a.WorkID].ActiveAttemptID != attemptID || !a.Reservation.Held {
			return orchestrator.State{}, orchestrator.ErrStale
		}
		success := runErr == nil && result.ExitCode == 0 && result.Interruption == nil && result.Termination == process.TerminationProcessGroupGone
		status := stoppedAttemptStatus(a, success)
		return orchestrator.Apply(current, orchestrator.Command{Kind: orchestrator.StopAttempt, ExpectedRevision: current.Revision, WorkID: a.WorkID, AttemptID: attemptID, AttemptStatus: status, At: time.Now(), Receipt: string(result.Termination), Usage: orchestrator.Usage{UnknownCost: result.Termination != process.TerminationNotStarted}})
	})
	return err == nil
}

func stoppedAttemptStatus(attempt orchestrator.Attempt, success bool) orchestrator.AttemptStatus {
	if attempt.Status == orchestrator.AttemptParking {
		return orchestrator.AttemptParked
	}
	if attempt.Status == orchestrator.AttemptCancelling {
		return orchestrator.AttemptCancelled
	}
	if success {
		return orchestrator.AttemptSucceeded
	}
	return orchestrator.AttemptFailed
}

// cancel persists intent before signaling. The caller joins outside the daemon mutex.
func (e *managedExecution) cancel(workID, attemptID, reason string, cancel context.CancelFunc, done <-chan struct{}) error {
	if cancel == nil || done == nil {
		return errors.New("cancellation requires an exact worker handle")
	}
	if err := e.transition(workID, attemptID, orchestrator.CancelAttempt, reason); err != nil {
		return err
	}
	cancel()
	<-done
	state, err := e.store.State()
	if err != nil {
		return err
	}
	attempt, ok := state.Attempts[attemptID]
	if !ok || attempt.WorkID != workID {
		return orchestrator.ErrStale
	}
	if attempt.Reservation.Held {
		return fmt.Errorf("%w: attempt %s retains its reservation", process.ErrTerminationUncertain, attemptID)
	}
	return nil
}

func (e *managedExecution) question(question orchestrator.Question, cancel context.CancelFunc, done <-chan struct{}) error {
	if cancel == nil || done == nil {
		return errors.New("question parking requires an exact worker handle")
	}
	if err := transactControl(e.store, func(state orchestrator.State) (orchestrator.State, error) {
		return orchestrator.Apply(state, orchestrator.Command{Kind: orchestrator.AskQuestion, ExpectedRevision: state.Revision, Question: question})
	}); err != nil {
		return err
	}
	cancel()
	<-done
	state, err := e.store.State()
	if err != nil {
		return err
	}
	if state.Attempts[question.AttemptID].Reservation.Held {
		return process.ErrTerminationUncertain
	}
	return nil
}
