package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func (m *mockService) CompanionSnapshot() CompanionInfo {
	return CompanionInfo{Health: "ok", Name: "Kotik"}
}
func (m *mockService) ReadCompanionItem(_ string, _ uint64, _ bool) (CompanionInfo, error) {
	return m.CompanionSnapshot(), nil
}
func (m *mockService) CatchUpCompanion(_ uint64) (CompanionInfo, error) {
	return m.CompanionSnapshot(), nil
}
func (m *mockService) PostCompanionMessage(_ CompanionMessageRequest) (AssistantThreadInfo, error) {
	return AssistantThreadInfo{ID: "k0123456789abcdef", Purpose: "companion"}, nil
}
func (m *mockTerminalService) CompanionSnapshot() CompanionInfo { return CompanionInfo{} }
func (m *mockTerminalService) ReadCompanionItem(_ string, _ uint64, _ bool) (CompanionInfo, error) {
	return CompanionInfo{}, nil
}
func (m *mockTerminalService) CatchUpCompanion(_ uint64) (CompanionInfo, error) {
	return CompanionInfo{}, nil
}
func (m *mockTerminalService) PostCompanionMessage(_ CompanionMessageRequest) (AssistantThreadInfo, error) {
	return AssistantThreadInfo{}, nil
}

func TestCompanionHandlers(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		status                   int
		errorText                string
	}{
		{"snapshot", "GET", "/api/assistant/companion", "", 200, ""},
		{"read", "PATCH", "/api/assistant/companion/items/item", `{"revision":1,"read":true}`, 200, ""},
		{"unread", "PATCH", "/api/assistant/companion/items/item", `{"revision":1,"read":false}`, 200, ""},
		{"missing read", "PATCH", "/api/assistant/companion/items/item", `{"revision":1}`, 400, ""},
		{"catch up", "PATCH", "/api/assistant/companion", `{"revision":1}`, 200, ""},
		{"missing revision", "PATCH", "/api/assistant/companion", `{}`, 400, ""},
		{"briefing", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":"What needs me?"}`, 202, ""},
		{"client evidence", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":"Explain","context":"fake evidence"}`, 400, ""},
		{"autonomy", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":"Explain","autonomy":"full"}`, 400, ""},
		{"empty", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":" "}`, 400, "text must be non-blank and at most 32 KiB; ticket_ids must contain no more than five IDs"},
		{"text at limit", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":"` + strings.Repeat("a", assistantTextMax) + `"}`, 202, ""},
		{"oversized text", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":"` + strings.Repeat("a", assistantTextMax+1) + `"}`, 400, "text must be non-blank and at most 32 KiB; ticket_ids must contain no more than five IDs"},
		{"oversized UTF-8 text", "POST", "/api/assistant/companion/messages", `{"kind":"briefing","text":"` + strings.Repeat("é", assistantTextMax/2+1) + `"}`, 400, "text must be non-blank and at most 32 KiB; ticket_ids must contain no more than five IDs"},
		{"too many tickets", "POST", "/api/assistant/companion/messages", `{"kind":"investigation","text":"Explain","ticket_ids":["a","b","c","d","e","f"]}`, 400, "text must be non-blank and at most 32 KiB; ticket_ids must contain no more than five IDs"},
		{"bad kind", "POST", "/api/assistant/companion/messages", `{"kind":"execute","text":"Run"}`, 400, ""},
		{"trailing body", "PATCH", "/api/assistant/companion", `{"revision":1}{}`, 400, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{svc: &mockService{}}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/assistant/companion", s.handleCompanion)
			mux.HandleFunc("PATCH /api/assistant/companion", s.handleCompanionCatchUp)
			mux.HandleFunc("PATCH /api/assistant/companion/items/{id}", s.handleCompanionRead)
			mux.HandleFunc("POST /api/assistant/companion/messages", s.handleCompanionMessage)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.errorText != "" {
				require.JSONEq(t, `{"error":"`+tc.errorText+`"}`, w.Body.String())
			}
		})
	}
}
