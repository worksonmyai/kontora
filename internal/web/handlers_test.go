package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockService implements TicketService for handler tests.
type mockService struct {
	tickets    []TicketInfo
	getTicket  *TicketInfo
	getErr     error
	actionFn   func(id string) error
	createFn   func(req CreateTicketRequest) (TicketInfo, error)
	uploadFn   func(content []byte) (TicketInfo, error)
	deleteFn   func(id string) error
	initFn     func(id string, req InitTicketRequest) error
	updateFn   func(id string, req UpdateTicketRequest) error
	logsFn     func(id, stage string) (string, error)
	configInfo ConfigInfo
}

func (m *mockService) ListTickets() []TicketInfo { return m.tickets }
func (m *mockService) RunningAgents() int        { return 0 }
func (m *mockService) GetTicket(id string) (TicketInfo, error) {
	if m.getErr != nil {
		return TicketInfo{}, m.getErr
	}
	if m.getTicket != nil {
		return *m.getTicket, nil
	}
	for _, t := range m.tickets {
		if t.ID == id {
			return t, nil
		}
	}
	return TicketInfo{}, ErrTicketNotFound
}
func (m *mockService) CreateTicket(req CreateTicketRequest) (TicketInfo, error) {
	if m.createFn != nil {
		return m.createFn(req)
	}
	return TicketInfo{}, nil
}
func (m *mockService) UploadTicket(content []byte) (TicketInfo, error) {
	if m.uploadFn != nil {
		return m.uploadFn(content)
	}
	return TicketInfo{}, nil
}
func (m *mockService) GetConfig() ConfigInfo { return m.configInfo }
func (m *mockService) DeleteTicket(id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(id)
	}
	return nil
}
func (m *mockService) PauseTicket(id string) error          { return m.actionFn(id) }
func (m *mockService) RetryTicket(id string) error          { return m.actionFn(id) }
func (m *mockService) SkipStage(id string) error            { return m.actionFn(id) }
func (m *mockService) SetStage(id string, _ string) error   { return m.actionFn(id) }
func (m *mockService) MoveTicket(id string, _ string) error { return m.actionFn(id) }
func (m *mockService) InitTicket(id string, req InitTicketRequest) error {
	if m.initFn != nil {
		return m.initFn(id, req)
	}
	return nil
}
func (m *mockService) UpdateTicket(id string, req UpdateTicketRequest) error {
	if m.updateFn != nil {
		return m.updateFn(id, req)
	}
	return nil
}
func (m *mockService) GetLogs(id, stage string) (string, error) {
	if m.logsFn != nil {
		return m.logsFn(id, stage)
	}
	return "", nil
}
func (m *mockService) Subscribe() (<-chan TicketEvent, func()) { return nil, func() {} }
func (m *mockService) HasTerminalSession(_ string) bool        { return false }

// --- GET /api/tickets ---

func TestHandleListTickets_Empty(t *testing.T) {
	srv := startHandlerTestServer(t, &mockService{})

	res := get(t, srv, "/api/tickets")
	assert.Equal(t, http.StatusOK, res.statusCode)
	assert.Equal(t, "application/json", res.contentType)
	assert.JSONEq(t, `{"tickets":[],"running_agents":0}`, res.body)
}

func TestHandleListTickets_WithTickets(t *testing.T) {
	started := time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)
	svc := &mockService{
		tickets: []TicketInfo{
			{ID: "t-001", Title: "Do thing", Status: "in_progress", Stage: "code", Pipeline: "default", Path: "~/projects/myrepo", Agent: "claude", Attempt: 0, StartedAt: &started, Stages: []string{"plan", "code"}},
			{ID: "t-002", Title: "Other thing", Status: "todo", Stage: "plan", Pipeline: "default"},
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets")
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result struct{ Tickets []TicketInfo }
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Len(t, result.Tickets, 2)
	assert.Equal(t, "t-001", result.Tickets[0].ID)
	assert.Equal(t, "in_progress", result.Tickets[0].Status)
	assert.Equal(t, []string{"plan", "code"}, result.Tickets[0].Stages)
}

// --- GET /api/tickets/{id} ---

func TestHandleGetTicket_Found(t *testing.T) {
	svc := &mockService{
		tickets: []TicketInfo{{ID: "t-001", Title: "Do thing", Status: "in_progress", Body: "ticket body here"}},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/t-001")
	assert.Equal(t, http.StatusOK, res.statusCode)
	assert.Equal(t, "application/json", res.contentType)

	var tkt TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &tkt))
	assert.Equal(t, "t-001", tkt.ID)
	assert.Equal(t, "ticket body here", tkt.Body)
}

func TestHandleGetTicket_NotFound(t *testing.T) {
	svc := &mockService{getErr: ErrTicketNotFound}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/nonexistent")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

// --- DELETE /api/tickets/{id} ---

func TestHandleDeleteTicket_Success(t *testing.T) {
	svc := &mockService{
		deleteFn: func(id string) error {
			assert.Equal(t, "t-001", id)
			return nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := del(t, srv, "/api/tickets/t-001")
	assert.Equal(t, http.StatusNoContent, res.statusCode)
	assert.Empty(t, strings.TrimSpace(res.body))
}

func TestHandleDeleteTicket_NotFound(t *testing.T) {
	svc := &mockService{
		deleteFn: func(_ string) error { return ErrTicketNotFound },
	}
	srv := startHandlerTestServer(t, svc)

	res := del(t, srv, "/api/tickets/missing")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandleDeleteTicket_RequiresConfirmation(t *testing.T) {
	srv := startHandlerTestServer(t, &mockService{})

	res := delNoConfirm(t, srv, "/api/tickets/t-001")
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "missing delete confirmation")
}

// --- POST /api/tickets/{id}/pause ---

func TestHandlePause_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Title: "Do thing", Status: "paused"}
	svc := &mockService{
		tickets:  []TicketInfo{tkt},
		actionFn: func(_ string) error { return nil },
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/pause", "")
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "t-001", result.ID)
}

func TestHandlePause_NotFound(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrTicketNotFound }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/pause", "")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandlePause_InvalidState(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrInvalidState }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/pause", "")
	assert.Equal(t, http.StatusConflict, res.statusCode)
}

// --- POST /api/tickets/{id}/retry ---

func TestHandleRetry_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Status: "todo"}
	svc := &mockService{
		tickets:  []TicketInfo{tkt},
		actionFn: func(_ string) error { return nil },
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/retry", "")
	assert.Equal(t, http.StatusOK, res.statusCode)
}

func TestHandleRetry_NotFound(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrTicketNotFound }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/retry", "")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

// --- POST /api/tickets/{id}/skip ---

func TestHandleSkip_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Status: "todo", Stage: "step2"}
	svc := &mockService{
		tickets:  []TicketInfo{tkt},
		actionFn: func(_ string) error { return nil },
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/skip", "")
	assert.Equal(t, http.StatusOK, res.statusCode)
}

func TestHandleSkip_NotFound(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrTicketNotFound }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/skip", "")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

// --- POST /api/tickets/{id}/set-stage ---

func TestHandleSetStage_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Status: "todo", Stage: "implement"}
	svc := &mockService{
		tickets:  []TicketInfo{tkt},
		actionFn: func(_ string) error { return nil },
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/set-stage", `{"stage":"implement"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "t-001", result.ID)
}

func TestHandleSetStage_NotFound(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrTicketNotFound }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/set-stage", `{"stage":"implement"}`)
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandleSetStage_InvalidStage(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrInvalidState }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/set-stage", `{"stage":"nonexistent"}`)
	assert.Equal(t, http.StatusConflict, res.statusCode)
}

func TestHandleSetStage_BadJSON(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return nil }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/set-stage", `{bad json}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

func TestHandleSetStage_MissingStage(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return nil }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/set-stage", `{}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

// --- POST /api/tickets/{id}/move ---

func TestHandleMove_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Status: "todo"}
	svc := &mockService{
		tickets:  []TicketInfo{tkt},
		actionFn: func(_ string) error { return nil },
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/move", `{"status":"paused"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)
}

func TestHandleMove_InvalidStatus(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return ErrInvalidState }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/move", `{"status":"invalid"}`)
	assert.Equal(t, http.StatusConflict, res.statusCode)
}

func TestHandleMove_BadJSON(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return nil }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/move", `{bad json}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

func TestHandleMove_MissingStatus(t *testing.T) {
	svc := &mockService{actionFn: func(_ string) error { return nil }}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/move", `{}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

// --- POST /api/tickets (create) ---

func TestHandleCreateTicket_Success(t *testing.T) {
	svc := &mockService{
		createFn: func(req CreateTicketRequest) (TicketInfo, error) {
			return TicketInfo{ID: "tst-001", Title: req.Title, Status: "todo", Path: req.Path, Pipeline: req.Pipeline}, nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"My ticket","path":"~/projects/myrepo","pipeline":"default"}`)
	assert.Equal(t, http.StatusCreated, res.statusCode)

	var tkt TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &tkt))
	assert.Equal(t, "tst-001", tkt.ID)
	assert.Equal(t, "My ticket", tkt.Title)
	assert.Equal(t, "~/projects/myrepo", tkt.Path)
}

func TestHandleCreateTicket_MissingTitle(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "title is required")
}

func TestHandleCreateTicket_MissingPath(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"My ticket"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "path is required")
}

func TestHandleCreateTicket_BadJSON(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{bad json}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

func TestHandleCreateTicket_InvalidStatus(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"My ticket","path":"~/projects/myrepo","status":"bogus"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "status must be")
}

func TestHandleCreateTicket_NewlineInTitle(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"legit\nstatus: running","path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "newlines")
}

// --- POST /api/tickets/{id}/init ---

func TestHandleInit_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Title: "My ticket", Status: "open", Kontora: false}
	svc := &mockService{
		tickets: []TicketInfo{tkt},
	}
	svc.initFn = func(_ string, req InitTicketRequest) error {
		assert.Equal(t, "default", req.Pipeline)
		assert.Equal(t, "~/projects/myrepo", req.Path)
		svc.tickets[0].Kontora = true
		svc.tickets[0].Status = "todo"
		svc.tickets[0].Pipeline = req.Pipeline
		svc.tickets[0].Path = req.Path
		return nil
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"pipeline":"default","path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "t-001", result.ID)
	assert.True(t, result.Kontora)
	assert.Equal(t, "todo", result.Status)
	assert.Equal(t, "default", result.Pipeline)
	assert.Equal(t, "~/projects/myrepo", result.Path)
}

func TestHandleInit_NoPipeline(t *testing.T) {
	svc := &mockService{}
	svc.initFn = func(_ string, req InitTicketRequest) error {
		assert.Equal(t, "", req.Pipeline)
		assert.Equal(t, "~/projects/myrepo", req.Path)
		return nil
	}
	svc.getTicket = &TicketInfo{ID: "t-001", Kontora: true, Status: "todo", Path: "~/projects/myrepo"}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)
}

func TestHandleInit_MissingPath(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"pipeline":"default"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "path is required")
}

func TestHandleInit_NewlineInFields(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"pipeline":"default\nstatus: running","path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "newlines")
}

func TestHandleInit_BadJSON(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{bad json}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

func TestHandleInit_NotFound(t *testing.T) {
	svc := &mockService{
		initFn: func(_ string, _ InitTicketRequest) error {
			return ErrTicketNotFound
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"pipeline":"default","path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandleInit_AlreadyInitialized(t *testing.T) {
	svc := &mockService{
		initFn: func(_ string, _ InitTicketRequest) error {
			return ErrInvalidState
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"pipeline":"default","path":"~/projects/myrepo"}`)
	assert.Equal(t, http.StatusConflict, res.statusCode)
}

// --- PUT /api/tickets/{id} (update) ---

func TestHandleUpdateTicket_Success(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Title: "My ticket", Status: "open", Body: "old body"}
	svc := &mockService{
		tickets: []TicketInfo{tkt},
	}
	svc.updateFn = func(id string, req UpdateTicketRequest) error {
		assert.Equal(t, "t-001", id)
		require.NotNil(t, req.Body)
		assert.Equal(t, "new body", *req.Body)
		svc.tickets[0].Body = *req.Body
		return nil
	}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"body":"new body"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "t-001", result.ID)
	assert.Equal(t, "new body", result.Body)
}

func TestHandleUpdateTicket_NotFound(t *testing.T) {
	svc := &mockService{
		updateFn: func(_ string, _ UpdateTicketRequest) error {
			return ErrTicketNotFound
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/nonexistent", `{"body":"new body"}`)
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandleUpdateTicket_InvalidState(t *testing.T) {
	svc := &mockService{
		updateFn: func(_ string, _ UpdateTicketRequest) error {
			return ErrInvalidState
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"body":"new body"}`)
	assert.Equal(t, http.StatusConflict, res.statusCode)
}

func TestHandleUpdateTicket_BadJSON(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{bad json}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
}

func TestHandleUpdateTicket_NewlineInPipeline(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"pipeline":"default\nstatus: running"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "newlines")
}

func TestHandleUpdateTicket_NewlineInPath(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"path":"~/projects\nstatus: running"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "newlines")
}

func TestHandleUpdateTicket_AgentSuccess(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Title: "My ticket", Status: "open", Agent: "opus"}
	svc := &mockService{
		tickets: []TicketInfo{tkt},
	}
	svc.updateFn = func(id string, req UpdateTicketRequest) error {
		assert.Equal(t, "t-001", id)
		require.NotNil(t, req.Agent)
		assert.Equal(t, "opus", *req.Agent)
		svc.tickets[0].Agent = *req.Agent
		return nil
	}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"agent":"opus"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "opus", result.Agent)
}

func TestHandleUpdateTicket_AgentClear(t *testing.T) {
	tkt := TicketInfo{ID: "t-001", Title: "My ticket", Status: "open", Agent: "opus"}
	svc := &mockService{
		tickets: []TicketInfo{tkt},
	}
	svc.updateFn = func(_ string, req UpdateTicketRequest) error {
		require.NotNil(t, req.Agent)
		assert.Equal(t, "", *req.Agent)
		svc.tickets[0].Agent = ""
		return nil
	}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"agent":""}`)
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "", result.Agent)
}

func TestHandleUpdateTicket_AgentUnknown(t *testing.T) {
	svc := &mockService{
		updateFn: func(_ string, _ UpdateTicketRequest) error {
			return fmt.Errorf("%w %q", ErrUnknownAgent, "bad-agent")
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"agent":"bad-agent"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "unknown agent")
}

func TestHandleUpdateTicket_AgentNewline(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := put(t, srv, "/api/tickets/t-001", `{"agent":"claude\nevil"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "newlines")
}

// --- POST /api/tickets/upload ---

func TestHandleUploadTickets_SingleFile(t *testing.T) {
	svc := &mockService{
		uploadFn: func(_ []byte) (TicketInfo, error) {
			return TicketInfo{ID: "upl-001", Title: "Uploaded ticket", Status: "open"}, nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := postMultipart(t, srv, "/api/tickets/upload", map[string][]byte{
		"ticket.md": []byte("---\nid: upl-001\n---\n# Uploaded ticket\n"),
	})
	assert.Equal(t, http.StatusCreated, res.statusCode)

	var result struct {
		Tickets []TicketInfo
		Errors  []struct{ File, Error string }
	}
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Len(t, result.Tickets, 1)
	assert.Equal(t, "upl-001", result.Tickets[0].ID)
	assert.Empty(t, result.Errors)
}

func TestHandleUploadTickets_MultipleWithPartialFailure(t *testing.T) {
	callCount := 0
	svc := &mockService{
		uploadFn: func(_ []byte) (TicketInfo, error) {
			callCount++
			if callCount == 2 {
				return TicketInfo{}, fmt.Errorf("invalid ticket file: missing title")
			}
			return TicketInfo{ID: fmt.Sprintf("upl-%03d", callCount), Title: "Ticket", Status: "open"}, nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := postMultipart(t, srv, "/api/tickets/upload", map[string][]byte{
		"good1.md": []byte("---\nid: t1\n---\n# Good 1\n"),
		"bad.md":   []byte("---\nid: t2\n---\nno heading\n"),
		"good2.md": []byte("---\nid: t3\n---\n# Good 2\n"),
	})
	assert.Equal(t, http.StatusCreated, res.statusCode)

	var result struct {
		Tickets []TicketInfo
		Errors  []struct{ File, Error string }
	}
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Len(t, result.Tickets, 2)
	assert.Len(t, result.Errors, 1)
}

func TestHandleUploadTickets_NoFiles(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := postMultipart(t, srv, "/api/tickets/upload", map[string][]byte{})
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "no files")
}

func TestHandleUploadTickets_NonMdFile(t *testing.T) {
	svc := &mockService{}
	srv := startHandlerTestServer(t, svc)

	res := postMultipart(t, srv, "/api/tickets/upload", map[string][]byte{
		"readme.txt": []byte("not a markdown ticket"),
	})
	assert.Equal(t, http.StatusBadRequest, res.statusCode)

	var result struct {
		Tickets []TicketInfo
		Errors  []struct{ File, Error string }
	}
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Empty(t, result.Tickets)
	assert.Len(t, result.Errors, 1)
	assert.Contains(t, result.Errors[0].Error, ".md")
}

func TestHandleUploadTickets_InvalidContent(t *testing.T) {
	svc := &mockService{
		uploadFn: func(_ []byte) (TicketInfo, error) {
			return TicketInfo{}, fmt.Errorf("invalid ticket file: bad frontmatter")
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := postMultipart(t, srv, "/api/tickets/upload", map[string][]byte{
		"bad.md": []byte("no frontmatter at all"),
	})
	assert.Equal(t, http.StatusBadRequest, res.statusCode)

	var result struct {
		Tickets []TicketInfo
		Errors  []struct{ File, Error string }
	}
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Empty(t, result.Tickets)
	assert.Len(t, result.Errors, 1)
}

// --- GET /api/tickets/{id}/logs ---

func TestHandleGetLogs_Success(t *testing.T) {
	svc := &mockService{
		logsFn: func(id, stage string) (string, error) {
			assert.Equal(t, "t-001", id)
			assert.Equal(t, "plan", stage)
			return "plan stage log output", nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/t-001/logs?stage=plan")
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result map[string]string
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "plan stage log output", result["content"])
}

func TestHandleGetLogs_NoStage(t *testing.T) {
	svc := &mockService{
		logsFn: func(id, stage string) (string, error) {
			assert.Equal(t, "t-001", id)
			assert.Equal(t, "", stage)
			return "latest log output", nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/t-001/logs")
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result map[string]string
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "latest log output", result["content"])
}

func TestHandleGetLogs_NotFound(t *testing.T) {
	svc := &mockService{
		logsFn: func(_, _ string) (string, error) {
			return "", ErrTicketNotFound
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/nonexistent/logs")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandleGetLogs_LogNotFound(t *testing.T) {
	svc := &mockService{
		logsFn: func(_, _ string) (string, error) {
			return "", ErrLogNotFound
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/t-001/logs?stage=nonexistent")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

func TestHandleGetLogs_PathTraversal(t *testing.T) {
	svc := &mockService{
		logsFn: func(id, stage string) (string, error) {
			assert.Equal(t, "t-001", id)
			assert.Equal(t, "passwd", stage)
			return "", ErrLogNotFound
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/tickets/t-001/logs?stage=../../etc/passwd")
	assert.Equal(t, http.StatusNotFound, res.statusCode)
}

// --- GET /api/config ---

func TestHandleConfig(t *testing.T) {
	svc := &mockService{
		configInfo: ConfigInfo{
			Pipelines: []string{"default", "review"},
			Agents:    []string{"opus", "sonnet"},
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := get(t, srv, "/api/config")
	assert.Equal(t, http.StatusOK, res.statusCode)

	var cfg ConfigInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &cfg))
	assert.Equal(t, []string{"default", "review"}, cfg.Pipelines)
	assert.Equal(t, []string{"opus", "sonnet"}, cfg.Agents)
}

func TestHandleCreateTicket_WithAgent(t *testing.T) {
	svc := &mockService{
		createFn: func(req CreateTicketRequest) (TicketInfo, error) {
			assert.Equal(t, "opus", req.Agent)
			return TicketInfo{ID: "tst-001", Title: req.Title, Status: "todo", Agent: req.Agent}, nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"My ticket","path":"~/projects/myrepo","agent":"opus"}`)
	assert.Equal(t, http.StatusCreated, res.statusCode)

	var tkt TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &tkt))
	assert.Equal(t, "opus", tkt.Agent)
}

func TestHandleCreateTicket_WithBody(t *testing.T) {
	svc := &mockService{
		createFn: func(req CreateTicketRequest) (TicketInfo, error) {
			assert.Equal(t, "Ticket description here", req.Body)
			return TicketInfo{ID: "tst-001", Title: req.Title, Status: "todo", Path: req.Path, Body: req.Body}, nil
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"My ticket","path":"~/projects/myrepo","body":"Ticket description here"}`)
	assert.Equal(t, http.StatusCreated, res.statusCode)

	var tkt TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &tkt))
	assert.Equal(t, "Ticket description here", tkt.Body)
}

func TestHandleCreateTicket_UnknownAgent(t *testing.T) {
	svc := &mockService{
		createFn: func(_ CreateTicketRequest) (TicketInfo, error) {
			return TicketInfo{}, fmt.Errorf("%w %q", ErrUnknownAgent, "bad-agent")
		},
	}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets", `{"title":"My ticket","path":"~/projects/myrepo","agent":"bad-agent"}`)
	assert.Equal(t, http.StatusBadRequest, res.statusCode)
	assert.Contains(t, res.body, "unknown agent")
}

func TestHandleInit_WithAgent(t *testing.T) {
	svc := &mockService{}
	svc.initFn = func(_ string, req InitTicketRequest) error {
		assert.Equal(t, "opus", req.Agent)
		return nil
	}
	svc.getTicket = &TicketInfo{ID: "t-001", Kontora: true, Status: "todo", Agent: "opus"}
	srv := startHandlerTestServer(t, svc)

	res := post(t, srv, "/api/tickets/t-001/init", `{"pipeline":"default","path":"~/projects/myrepo","agent":"opus"}`)
	assert.Equal(t, http.StatusOK, res.statusCode)

	var result TicketInfo
	require.NoError(t, json.Unmarshal([]byte(res.body), &result))
	assert.Equal(t, "opus", result.Agent)
}

// --- GET /api/events (SSE) ---

func TestHandleSSE_StreamsEvents(t *testing.T) {
	broker := NewSSEBroker()
	srv := startHandlerTestServerWithBroker(t, &mockService{}, broker)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/api/events", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Wait for the handler to subscribe to the broker.
	require.Eventually(t, func() bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return len(broker.clients) > 0
	}, time.Second, 5*time.Millisecond)

	// Broadcast an event.
	broker.Broadcast(TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "t-001", Status: "in_progress"}})

	// Read the SSE event from the response body.
	scanner := bufio.NewScanner(resp.Body)
	var lines []string
	for scanner.Scan() {
		line := scanner.Text()
		lines = append(lines, line)
		if line == "" {
			break // End of SSE event (blank line delimiter).
		}
	}

	require.Len(t, lines, 3) // "event: ...", "data: ...", ""
	assert.Equal(t, "event: ticket_updated", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "data: "))

	var tkt TicketInfo
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &tkt))
	assert.Equal(t, "t-001", tkt.ID)
	assert.Equal(t, "in_progress", tkt.Status)
}

func TestHandleSSE_Disconnect(t *testing.T) {
	broker := NewSSEBroker()
	srv := startHandlerTestServerWithBroker(t, &mockService{}, broker)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+srv.Addr()+"/api/events", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	// Wait for the handler to subscribe.
	require.Eventually(t, func() bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return len(broker.clients) > 0
	}, time.Second, 5*time.Millisecond)

	// Cancel context to simulate client disconnect.
	cancel()
	resp.Body.Close()

	// Give the handler time to clean up.
	require.Eventually(t, func() bool {
		broker.mu.Lock()
		defer broker.mu.Unlock()
		return len(broker.clients) == 0
	}, time.Second, 5*time.Millisecond)

	// Broadcast should not panic after client disconnects.
	broker.Broadcast(TicketEvent{Type: "ticket_updated", Ticket: TicketInfo{ID: "t-001"}})
}

// --- helpers ---

type httpResult struct {
	statusCode  int
	contentType string
	body        string
}

func startHandlerTestServer(t *testing.T, svc TicketService) *Server {
	t.Helper()
	return startHandlerTestServerWithBroker(t, svc, NewSSEBroker())
}

func startHandlerTestServerWithBroker(t *testing.T, svc TicketService, broker *SSEBroker) *Server {
	t.Helper()
	srv := New(svc, broker, "127.0.0.1", 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, srv.Start())
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv
}

func get(t *testing.T, srv *Server, path string) httpResult {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s%s", srv.Addr(), path))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{statusCode: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body)}
}

func put(t *testing.T, srv *Server, path string, jsonBody string) httpResult {
	t.Helper()
	var bodyReader io.Reader
	if jsonBody != "" {
		bodyReader = strings.NewReader(jsonBody)
	}
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://%s%s", srv.Addr(), path), bodyReader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{statusCode: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body)}
}

func postMultipart(t *testing.T, srv *Server, path string, files map[string][]byte) httpResult {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, content := range files {
		part, err := w.CreateFormFile("files", name)
		require.NoError(t, err)
		_, err = part.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())

	resp, err := http.Post(fmt.Sprintf("http://%s%s", srv.Addr(), path), w.FormDataContentType(), &buf)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{statusCode: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body)}
}

func post(t *testing.T, srv *Server, path string, jsonBody string) httpResult {
	t.Helper()
	var bodyReader io.Reader
	if jsonBody != "" {
		bodyReader = strings.NewReader(jsonBody)
	}
	resp, err := http.Post(fmt.Sprintf("http://%s%s", srv.Addr(), path), "application/json", bodyReader)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{statusCode: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body)}
}

func del(t *testing.T, srv *Server, path string) httpResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://%s%s", srv.Addr(), path), nil)
	require.NoError(t, err)
	req.Header.Set("X-Kontora-Confirm", "delete-ticket-file")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{statusCode: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body)}
}

func delNoConfirm(t *testing.T, srv *Server, path string) httpResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("http://%s%s", srv.Addr(), path), nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return httpResult{statusCode: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: string(body)}
}
