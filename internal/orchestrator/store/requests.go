package store

import (
	"encoding/json"
	"fmt"

	"github.com/worksonmyai/kontora/internal/orchestrator"
)

// ExecuteRequest journals the response with the mutation. A retry precedes revision checking.
// The callback changes control state only and must not perform effects or call this store.
func (s *Store) ExecuteRequest(id, operatorID string, payload []byte, expectedRevision uint64, change func(orchestrator.State) (orchestrator.State, []byte, error)) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return nil, err
	}
	if id == "" || operatorID == "" || len(id) > 256 || len(payload) > 1<<20 || change == nil {
		return nil, fmt.Errorf("invalid operator request")
	}
	binding, err := json.Marshal(struct {
		BoardID    string
		OperatorID string
		Revision   uint64
		Payload    []byte
	}{s.state.BoardID, operatorID, expectedRevision, payload})
	if err != nil {
		return nil, err
	}
	fingerprint := digest(binding)
	if recorded, ok := s.state.Requests[id]; ok {
		if recorded.OperatorID != operatorID || recorded.Fingerprint != fingerprint {
			return nil, orchestrator.ErrConflict
		}
		return s.readArtifact(recorded.Response)
	}
	var response []byte
	err = s.transact(expectedRevision, func(state orchestrator.State) (orchestrator.State, error) {
		next, body, err := change(state)
		if err != nil {
			return orchestrator.State{}, err
		}
		if len(body) > 1<<20 {
			return orchestrator.State{}, fmt.Errorf("operator response exceeds limit")
		}
		ref, err := s.putArtifact(body)
		if err != nil {
			return orchestrator.State{}, err
		}
		if next.Requests == nil {
			next.Requests = map[string]orchestrator.ControlRequest{}
		}
		next.Requests[id] = orchestrator.ControlRequest{ID: id, OperatorID: operatorID, Fingerprint: fingerprint, Response: ref}
		response = append([]byte(nil), body...)
		return next, nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}
