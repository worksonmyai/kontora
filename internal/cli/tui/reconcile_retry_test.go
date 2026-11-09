package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestReconnectFailedReadsRetryOnConnectedTick(t *testing.T) {
	seed := web.TicketInfo{ID: "subject", Status: "in_progress", Body: "old body", Kontora: true}
	var lists, details atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := seed
		info.Status, info.Body = "done", "recovered body"
		switch r.URL.Path {
		case "/api/tickets":
			if lists.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []web.TicketInfo{info}})
		case "/api/tickets/subject":
			if details.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(info)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := newModel(&apiSource{client: remote.New(srv.URL, "")})
	m.list.setTickets([]web.TicketInfo{seed}, 1)
	m, _ = m.openDetail(seed)
	runReconcileCmd(&m, applyReconcileMsg(&m, taskUpdatedMsg{Type: remote.SSEConnected}))
	require.Equal(t, viewDetail, m.view)
	require.Equal(t, seed, m.detail.ticket)
	require.Equal(t, []web.TicketInfo{seed}, m.list.tickets)
	require.Error(t, m.list.err)
	require.Error(t, m.detail.err)
	require.True(t, m.listRetry)
	require.True(t, m.detailRetry)

	cmd := applyReconcileMsg(&m, tickMsg{})
	batch := cmd().(tea.BatchMsg)
	require.Len(t, batch, 4, "config, list retry, detail retry, next tick")
	runReconcileCmd(&m, batch[1])
	runReconcileCmd(&m, batch[2])
	require.Equal(t, "done", m.list.tickets[0].Status)
	require.Equal(t, "done", m.detail.ticket.Status)
	require.Equal(t, "recovered body", m.detail.ticket.Body)
	require.False(t, m.listRetry)
	require.False(t, m.detailRetry)
	require.NoError(t, m.list.err)
	require.NoError(t, m.detail.err)
	require.Equal(t, int32(2), lists.Load())
	require.Equal(t, int32(2), details.Load())

	cmd = applyReconcileMsg(&m, tickMsg{})
	require.Len(t, cmd().(tea.BatchMsg), 2, "healthy connected ticks do not poll tickets")
}

func TestPrimaryTUIIgnoresOtherSourceEvents(t *testing.T) {
	seed := web.TicketInfo{ID: "subject", Status: "open", Body: "primary body"}
	for _, event := range []web.TicketEvent{
		{Type: "secondary_ticket_updated", Ticket: web.TicketInfo{ID: seed.ID, Status: "done", Source: "other"}},
		{Type: "secondary_ticket_deleted", Ticket: web.TicketInfo{ID: seed.ID, Source: "other"}},
		{Type: "ticket_updated", Ticket: web.TicketInfo{ID: seed.ID, Status: "done", Source: "other"}},
		{Type: "config_updated", Ticket: web.TicketInfo{ID: seed.ID, Status: "done"}},
	} {
		t.Run(event.Type+event.Ticket.Source, func(t *testing.T) {
			m := newModel(&mockSource{connected: true})
			m.list.setTickets([]web.TicketInfo{seed}, 0)
			m, _ = m.openDetail(seed)
			require.Nil(t, applyReconcileMsg(&m, taskUpdatedMsg(event)))
			require.Equal(t, []web.TicketInfo{seed}, m.list.tickets)
			require.Equal(t, seed, m.detail.ticket)
			require.Equal(t, viewDetail, m.view)
		})
	}
}
