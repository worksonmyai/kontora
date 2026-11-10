package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/ticket"
)

type secondaryMockService struct {
	mockService
	sources        []TicketSourceInfo
	tickets        map[string]TicketInfo
	calls          []string
	mutations      []SecondaryMutation
	updates        []UpdateTicketRequest
	mutationResult *TicketInfo
	err            error
}

func newSecondaryMock() *secondaryMockService {
	return &secondaryMockService{
		mockService: mockService{getTicket: &TicketInfo{ID: "same", Title: "primary"}, tickets: []TicketInfo{{ID: "same", Title: "primary"}}},
		sources: []TicketSourceInfo{
			{ID: "team", Path: "/team", Available: true},
			{ID: "other", Path: "/other", Available: true},
			{ID: "offline", Path: "/offline", Error: "directory missing"},
		},
		tickets: map[string]TicketInfo{
			"team/same":  {ID: "same", Title: "team namesake", Status: "in_progress"},
			"other/same": {ID: "same", Title: "other namesake", Status: "archived"},
		},
	}
}

func (m *secondaryMockService) ListTicketSources() []TicketSourceInfo { return m.sources }
func (m *secondaryMockService) ListSecondaryTickets(source string) ([]TicketInfo, error) {
	m.calls = append(m.calls, "list "+source)
	if m.err != nil {
		return nil, m.err
	}
	var result []TicketInfo
	for key, tkt := range m.tickets {
		if strings.HasPrefix(key, source+"/") {
			result = append(result, tkt)
		}
	}
	return result, nil
}
func (m *secondaryMockService) GetSecondaryTicket(source, id string) (TicketInfo, error) {
	m.calls = append(m.calls, "get "+source+"/"+id)
	if m.err != nil {
		return TicketInfo{}, m.err
	}
	tkt, ok := m.tickets[source+"/"+id]
	if !ok {
		return TicketInfo{}, ErrTicketNotFound
	}
	return tkt, nil
}
func (m *secondaryMockService) UpdateSecondaryTicket(source, id string, req UpdateTicketRequest) (TicketInfo, error) {
	m.calls = append(m.calls, "update "+source+"/"+id)
	m.updates = append(m.updates, req)
	if m.err != nil {
		return TicketInfo{}, m.err
	}
	tkt := m.tickets[source+"/"+id]
	if req.Body != nil {
		tkt.Body = *req.Body
	}
	m.tickets[source+"/"+id] = tkt
	if m.mutationResult != nil {
		return *m.mutationResult, nil
	}
	return tkt, nil
}
func (m *secondaryMockService) ClaimSecondaryTicket(source, id, revision string) (TicketInfo, error) {
	m.calls = append(m.calls, "claim "+source+"/"+id+"@"+revision)
	return TicketInfo{ID: id, Title: "claimed", Status: "open"}, m.err
}
func (m *secondaryMockService) MutateSecondaryTicket(source, id string, op SecondaryMutation) (TicketInfo, error) {
	m.calls = append(m.calls, "mutate "+source+"/"+id)
	m.mutations = append(m.mutations, op)
	if m.mutationResult != nil {
		return *m.mutationResult, m.err
	}
	return m.tickets[source+"/"+id], m.err
}

func secondaryTestHandler(t *testing.T, svc TicketService) http.Handler {
	t.Helper()
	srv, err := New(svc, NewSSEBroker(), "127.0.0.1", 0, "", nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return srv.httpSrv.Handler
}

func secondaryRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestSecondarySourceQualifiedNamesakes(t *testing.T) {
	m := newSecondaryMock()
	m.updateFn = func(string, UpdateTicketRequest) error { t.Fatal("secondary update reached primary"); return nil }
	m.noteFn = func(string, AddNoteRequest) error { t.Fatal("secondary note reached primary"); return nil }
	handler := secondaryTestHandler(t, m)
	for _, tc := range []struct{ path, title string }{
		{"/api/tickets/same", "primary"},
		{"/api/sources/team/tickets/same", "team namesake"},
		{"/api/sources/other/tickets/same", "other namesake"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := secondaryRequest(handler, http.MethodGet, tc.path, "")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var tkt TicketInfo
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tkt))
			require.Equal(t, tc.title, tkt.Title)
		})
	}
	w := secondaryRequest(handler, http.MethodPut, "/api/sources/team/tickets/same", `{"body":"new body"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, []string{"get team/same", "get other/same", "update team/same"}, m.calls)
	require.Equal(t, "new body", m.tickets["team/same"].Body)
	require.Empty(t, m.tickets["other/same"].Body)
	require.Empty(t, m.getTicket.Body)
	w = secondaryRequest(handler, http.MethodPost, "/api/sources/team/tickets/same/note", `{"text":"local note"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, []SecondaryMutation{{Action: SecondaryAddNote, Text: "local note"}}, m.mutations)
}

func TestSecondarySourcesAndLists(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	w := secondaryRequest(handler, http.MethodGet, "/api/ticket-sources", "")
	require.Equal(t, http.StatusOK, w.Code)
	var sources []TicketSourceInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sources))
	require.Equal(t, m.sources, sources)
	require.Contains(t, w.Body.String(), `"available":false`)
	require.Contains(t, w.Body.String(), `"error":"directory missing"`)
	for _, tc := range []struct{ source, status string }{{"team", "in_progress"}, {"other", "archived"}} {
		w := secondaryRequest(handler, http.MethodGet, "/api/sources/"+tc.source+"/tickets", "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var list []TicketInfo
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &list))
		require.Len(t, list, 1)
		require.Equal(t, tc.status, list[0].Status)
	}
	m.tickets = nil
	w = secondaryRequest(handler, http.MethodGet, "/api/sources/team/tickets", "")
	require.JSONEq(t, `[]`, w.Body.String())
	optional := secondaryTestHandler(t, &mockService{})
	w = secondaryRequest(optional, http.MethodGet, "/api/ticket-sources", "")
	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `[]`, w.Body.String())
	w = secondaryRequest(optional, http.MethodGet, "/api/sources/team/tickets/same", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestSecondaryMutationRoutes(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	for _, tc := range []struct {
		method, suffix, body string
		op                   SecondaryMutation
	}{
		{"POST", "/note", `{"text":"note","author":"author","parent":"n001"}`, SecondaryMutation{Action: SecondaryAddNote, Text: "note", Author: "author", Parent: "n001"}},
		{"PATCH", "/notes/n001", `{"text":"edited"}`, SecondaryMutation{Action: SecondaryEditNote, NoteID: "n001", Text: "edited"}},
		{"DELETE", "/notes/n001", "", SecondaryMutation{Action: SecondaryDeleteNote, NoteID: "n001"}},
		{"POST", "/notes/n001/reactions", `{"emoji":"👍","actor":"actor"}`, SecondaryMutation{Action: SecondarySetReaction, NoteID: "n001", Emoji: "👍", Actor: "actor", On: true}},
		{"DELETE", "/notes/n001/reactions/%F0%9F%91%8D?actor=actor", "", SecondaryMutation{Action: SecondarySetReaction, NoteID: "n001", Emoji: "👍", Actor: "actor"}},
		{"POST", "/summary", `{"text":"summary"}`, SecondaryMutation{Action: SecondarySetSummary, Text: "summary"}},
		{"POST", "/dep", `{"related":["same"]}`, SecondaryMutation{Action: SecondaryAddDependency, RelatedIDs: []string{"same"}}},
		{"POST", "/undep", `{"related":["same"]}`, SecondaryMutation{Action: SecondaryRemoveDependency, RelatedIDs: []string{"same"}}},
		{"POST", "/link", `{"related":["same","local"]}`, SecondaryMutation{Action: SecondaryLink, RelatedIDs: []string{"same", "local"}}},
		{"POST", "/unlink", `{"related":["same","local"]}`, SecondaryMutation{Action: SecondaryUnlink, RelatedIDs: []string{"same", "local"}}},
		{"POST", "/parent", `{"parent":"same"}`, SecondaryMutation{Action: SecondarySetParent, Parent: "same"}},
		{"POST", "/unparent", "", SecondaryMutation{Action: SecondaryClearParent}},
		{"PUT", "/children", `{"children":["same","local"]}`, SecondaryMutation{Action: SecondarySetChildOrder, Children: []string{"same", "local"}}},
		{"PUT", "/children", `{"children":[]}`, SecondaryMutation{Action: SecondarySetChildOrder, Children: []string{}}},
	} {
		t.Run(tc.method+tc.suffix, func(t *testing.T) {
			m.calls, m.mutations = nil, nil
			w := secondaryRequest(handler, tc.method, "/api/sources/team/tickets/same"+tc.suffix, tc.body)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Equal(t, []SecondaryMutation{tc.op}, m.mutations)
			require.Equal(t, []string{"mutate team/same"}, m.calls)
			require.Contains(t, w.Body.String(), "team namesake")
		})
	}
}

func TestSecondaryRefusesLifecycleAndArtifacts(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	for _, tc := range []struct{ method, suffix string }{
		{"DELETE", ""}, {"POST", "/pause"}, {"POST", "/retry"}, {"POST", "/run"},
		{"POST", "/skip"}, {"POST", "/set-stage"}, {"POST", "/schedule"}, {"POST", "/move"},
		{"POST", "/archive"}, {"POST", "/restore"}, {"POST", "/init"},
		{"POST", "/plannotator-review"}, {"POST", "/plannotator-annotate"},
		{"GET", "/logs"}, {"GET", "/activity"}, {"GET", "/changes"}, {"GET", "/chain"}, {"GET", "/cost"},
		{"POST", "/unknown"}, {"PATCH", ""}, {"OPTIONS", "/notes/n001"},
	} {
		t.Run(tc.method+tc.suffix, func(t *testing.T) {
			w := secondaryRequest(handler, tc.method, "/api/sources/team/tickets/same"+tc.suffix, `{"status":"todo"}`)
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			require.Contains(t, w.Header().Get("Content-Type"), "application/json")
			require.Contains(t, w.Body.String(), ErrSecondaryOperationRefused.Error())
		})
	}
	for _, path := range []string{"/api/sources/team/tickets", "/api/sources/team/tickets/upload", "/api/sources/team/assistant", "/api/sources/team"} {
		w := secondaryRequest(handler, "POST", path, `{}`)
		require.Equal(t, http.StatusForbidden, w.Code, path)
	}
	require.Empty(t, m.calls)
}

func TestSecondaryStrictJSON(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	routes := []struct{ method, suffix, body string }{
		{"PUT", "", `{"description":"edit"}`},
		{"POST", "/claim", `{"revision":"r1"}`},
		{"POST", "/note", `{"text":"edit"}`},
		{"PATCH", "/notes/n001", `{"text":"edit"}`},
		{"DELETE", "/notes/n001", `{}`},
		{"POST", "/notes/n001/reactions", `{"emoji":"👍"}`},
		{"DELETE", "/notes/n001/reactions/%F0%9F%91%8D", `{}`},
		{"POST", "/summary", `{"text":"edit"}`},
		{"POST", "/dep", `{"related":["local"]}`},
		{"POST", "/undep", `{"related":["local"]}`},
		{"POST", "/link", `{"related":["local"]}`},
		{"POST", "/unlink", `{"related":["local"]}`},
		{"POST", "/parent", `{"parent":"local"}`},
		{"POST", "/unparent", `{}`},
		{"PUT", "/children", `{"children":["local"]}`},
	}
	for _, route := range routes {
		for _, field := range []string{"status", "scheduled_at", "kontora", "stage", "attempt", "history", "managed", "invocation_id", "claimed_by", "kind", "archived_at", "annotation_return_status"} {
			t.Run(route.method+route.suffix+"/"+field, func(t *testing.T) {
				body := strings.TrimSuffix(route.body, "}")
				if body != "{" {
					body += ","
				}
				body += fmt.Sprintf(`%q:null}`, field)
				w := secondaryRequest(handler, route.method, "/api/sources/team/tickets/same"+route.suffix, body)
				require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			})
		}
		for _, body := range []string{route.body + ` {}`, route.body + ` null`, route.body + ` garbage`, `null`, `[]`, `{"text":`} {
			w := secondaryRequest(handler, route.method, "/api/sources/team/tickets/same"+route.suffix, body)
			require.Equal(t, http.StatusBadRequest, w.Code, route.method+route.suffix+": "+body)
		}
	}
	require.Empty(t, m.calls)
	require.Empty(t, m.updates)
	require.Empty(t, m.mutations)
}

func TestSecondaryValidation(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	for _, tc := range []struct{ method, suffix, body string }{
		{"PUT", "", `{"body":"body","description":"description"}`},
		{"PUT", "", `{"agent":"agent\nstatus: todo"}`},
		{"PUT", "", `{"pipeline":"p\rstatus: todo"}`},
		{"PUT", "", `{"path":"/repo\nstatus: todo"}`},
		{"PUT", "", `{"branch":"bad branch"}`},
		{"PUT", "", `{"base_branch":"bad branch"}`},
		{"POST", "/claim", `{}`}, {"POST", "/claim", `{"revision":" "}`},
		{"POST", "/note", `{}`}, {"PATCH", "/notes/n001", `{}`}, {"POST", "/summary", `{}`},
		{"POST", "/notes/n001/reactions", `{}`},
		{"POST", "/dep", `{"related":[]}`}, {"POST", "/undep", `{"related":["a","b"]}`},
		{"POST", "/link", `{"related":[""]}`}, {"POST", "/unlink", `{}`},
		{"POST", "/parent", `{}`}, {"PUT", "/children", `{"children":[""]}`},
	} {
		t.Run(tc.method+tc.suffix+tc.body, func(t *testing.T) {
			w := secondaryRequest(handler, tc.method, "/api/sources/team/tickets/same"+tc.suffix, tc.body)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
	require.Empty(t, m.calls)
	w := secondaryRequest(handler, "PUT", "/api/sources/team/tickets/same", `{"description":"# New title","pipeline":"pipeline","path":"/repo","agent":"agent","branch":" branch ","base_branch":" main ","notify":["done"],"notify_channels":["desktop"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, m.updates, 1)
	require.Equal(t, "branch", *m.updates[0].Branch)
	require.Equal(t, "main", *m.updates[0].BaseBranch)
}

func TestSecondaryMutationReturnsCoherentServiceResult(t *testing.T) {
	for _, tc := range []struct{ method, suffix, body, call string }{
		{"PUT", "", `{"description":"# Written"}`, "update team/same"},
		{"POST", "/note", `{"text":"Written note"}`, "mutate team/same"},
		{"POST", "/notes/n001/reactions", `{"emoji":"👍"}`, "mutate team/same"},
	} {
		t.Run(tc.method+tc.suffix, func(t *testing.T) {
			m := newSecondaryMock()
			want := TicketInfo{
				Source: "team", ID: "same", Title: "Coherent post-write", Revision: "reviewed-revision",
				Body: "# Coherent post-write\n", Description: new("# Coherent post-write\n"), DescriptionTail: new(""),
				Notes: []NoteInfo{{ID: "n001", Text: "Written note", Reactions: []ReactionInfo{{Emoji: "👍", Actors: []string{"reader"}}}}},
			}
			m.mutationResult = &want
			w := secondaryRequest(secondaryTestHandler(t, m), tc.method, "/api/sources/team/tickets/same"+tc.suffix, tc.body)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var got TicketInfo
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			require.Equal(t, want, got)
			require.Equal(t, []string{tc.call}, m.calls, "response must not trigger a second detail observation")
		})
	}
}

func TestSecondaryClaimReturnsServiceIdentity(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	w := secondaryRequest(handler, "POST", "/api/sources/team/tickets/same/claim", `{"revision":"r1"}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var tkt TicketInfo
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &tkt))
	require.Equal(t, "claimed", tkt.Title)
	require.Equal(t, "open", tkt.Status)
	require.Equal(t, []string{"claim team/same@r1"}, m.calls)
}

func TestSecondarySourceAndServiceErrors(t *testing.T) {
	m := newSecondaryMock()
	handler := secondaryTestHandler(t, m)
	for _, tc := range []struct {
		source string
		code   int
	}{{"missing", http.StatusNotFound}, {"offline", http.StatusServiceUnavailable}} {
		for _, route := range []struct{ method, suffix, body string }{
			{"GET", "", ""}, {"GET", "/same", ""}, {"PUT", "/same", `{}`},
			{"POST", "/same/note", `{"text":"note"}`}, {"POST", "/same/claim", `{"revision":"r1"}`},
			{"POST", "/same/run", ""},
		} {
			w := secondaryRequest(handler, route.method, "/api/sources/"+tc.source+"/tickets"+route.suffix, route.body)
			require.Equal(t, tc.code, w.Code, w.Body.String())
		}
	}
	require.Empty(t, m.calls)
	for _, tc := range []struct {
		err  error
		code int
	}{
		{ErrTicketNotFound, http.StatusNotFound}, {ErrTicketSourceNotFound, http.StatusNotFound},
		{ErrTicketSourceUnavailable, http.StatusServiceUnavailable},
		{ErrInvalidState, http.StatusConflict}, {ErrUnknownAgent, http.StatusBadRequest},
		{ErrUnknownPipeline, http.StatusBadRequest}, {ErrInvalidNotify, http.StatusBadRequest},
		{ErrNoteNotFound, http.StatusNotFound}, {ErrNoteNested, http.StatusConflict},
		{ticket.ErrManaged, http.StatusForbidden}, {ErrSecondaryOperationRefused, http.StatusForbidden},
		{errors.New("disk failed"), http.StatusInternalServerError},
	} {
		m.err = fmt.Errorf("secondary operation: %w", tc.err)
		for _, route := range []struct{ method, suffix, body string }{
			{"GET", "", ""}, {"GET", "/same", ""}, {"PUT", "/same", `{}`},
			{"POST", "/same/note", `{"text":"note"}`}, {"POST", "/same/claim", `{"revision":"r1"}`},
		} {
			w := secondaryRequest(handler, route.method, "/api/sources/team/tickets"+route.suffix, route.body)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), tc.err.Error())
		}
	}
}
