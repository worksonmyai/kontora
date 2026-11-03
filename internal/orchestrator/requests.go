package orchestrator

import (
	"fmt"
	"reflect"
)

type ControlRequest struct {
	ID          string      `json:"id"`
	OperatorID  string      `json:"operator_id"`
	Fingerprint string      `json:"fingerprint"`
	Response    ArtifactRef `json:"response"`
}

func validateControlRequests(s State) error {
	for id, r := range s.Requests {
		if id == "" || r.ID != id || r.OperatorID == "" || !validDigest(r.Fingerprint) || !validDigest(r.Response.Hash) || r.Response.Size < 0 {
			return fmt.Errorf("invalid operator request %s", id)
		}
	}
	return nil
}

func validateControlRequestTransitions(before, after State) error {
	for id, r := range before.Requests {
		if !reflect.DeepEqual(r, after.Requests[id]) {
			return fmt.Errorf("recorded operator request changed %s", id)
		}
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}
