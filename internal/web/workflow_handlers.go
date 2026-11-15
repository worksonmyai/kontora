package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// decodeWorkflowRelease accepts exactly the named, non-null fields of a flat
// object. In particular encoding/json's last-key-wins and case folding are not
// appropriate for an identity-checked release request.
func decodeWorkflowRelease(w http.ResponseWriter, r *http.Request, out any, fields ...string) error {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || !utf8.Valid(data) {
		return fmt.Errorf("invalid release JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return fmt.Errorf("expected a release object")
	}
	allowed := make(map[string]bool, len(fields))
	for _, field := range fields {
		allowed[field] = true
	}
	seen := make(map[string]bool, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || seen[key] {
			return fmt.Errorf("unknown or duplicate release field")
		}
		seen[key] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("invalid release field %s", key)
		}
		if len(raw) > 0 && raw[0] == '"' {
			var value string
			if json.Unmarshal(raw, &value) != nil || strings.ContainsRune(value, 0) || !workflowReleaseUnicode(raw) {
				return fmt.Errorf("invalid release text")
			}
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || len(seen) != len(fields) {
		return fmt.Errorf("all release fields are required")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected one release object")
	}
	return json.Unmarshal(data, out)
}

// JSON syntax has already been checked. Reject unpaired UTF-16 escapes instead
// of letting encoding/json silently replace part of an idempotency payload.
func workflowReleaseUnicode(raw []byte) bool {
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		n, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n < 0xd800 || n > 0xdbff {
			continue
		}
		if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func (s *Server) handleWorkflowRelease(w http.ResponseWriter, r *http.Request) {
	var req WorkflowReleaseRequest
	if err := decodeWorkflowRelease(w, r, &req, "execution_id", "wait_id", "revision", "request_id", "reason"); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Operator, _ = OperatorIdentity(r.Context())
	svc, ok := s.svc.(WorkflowReleaseService)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "workflow release requires the daemon")
		return
	}
	receipt, err := svc.WorkflowRelease(r.PathValue("id"), req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleDeploymentRelease(w http.ResponseWriter, r *http.Request) {
	var req DeploymentReleaseRequest
	if err := decodeWorkflowRelease(w, r, &req, "repository", "environment", "commit", "event_id"); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.Operator, _ = OperatorIdentity(r.Context())
	svc, ok := s.svc.(WorkflowReleaseService)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "deployment release requires the daemon")
		return
	}
	receipt, err := svc.DeploymentRelease(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleWorkflowRestart(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, s.svc.WorkflowRestart)
}

func (s *Server) handleWorkflowRecover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExecutionID string  `json:"execution_id"`
		Revision    *uint64 `json:"revision"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.ExecutionID == "" || body.Revision == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "execution_id and revision are required"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected one JSON object"})
		return
	}
	s.handleAction(w, r, func(id string) error { return s.svc.WorkflowRecover(id, body.ExecutionID, *body.Revision) })
}
