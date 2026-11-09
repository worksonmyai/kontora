package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/web"
)

func applyReconcileMsg(m *model, msg tea.Msg) tea.Cmd {
	updated, cmd := m.Update(msg)
	*m = updated.(model)
	return cmd
}

func runReconcileCmd(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			runReconcileCmd(m, child)
		}
		return
	}
	runReconcileCmd(m, applyReconcileMsg(m, msg))
}

func receiveReconcileEvent(t *testing.T, events <-chan web.TicketEvent) web.TicketEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		require.True(t, ok, "subscription closed")
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("missing stream event")
		return web.TicketEvent{}
	}
}

// The HTTP fixture uses the production broker's overflow and close behavior.
// The first stream stops reading the broker to reproduce a stalled SSE writer.
func TestReconnectReconcilesListAndOpenDetail(t *testing.T) {
	for _, mode := range []string{"overflow", "network"} {
		t.Run(mode, func(t *testing.T) {
			broker := web.NewSSEBroker()
			var opens, lists, details atomic.Int32
			var changed atomic.Bool
			gate, returned, allowReconnect := make(chan struct{}), make(chan struct{}), make(chan struct{})
			seed := web.TicketInfo{ID: "subject", Status: "in_progress", Body: "old body", Kontora: true}
			current := func() web.TicketInfo {
				info := seed
				if changed.Load() {
					info.Status, info.Body = "done", "gap body"
				}
				return info
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				switch r.URL.Path {
				case "/api/tickets":
					lists.Add(1)
					items := []web.TicketInfo{current()}
					running := 0
					if changed.Load() {
						items = append(items, web.TicketInfo{ID: "gap-added", Status: "open"})
					} else {
						items = append(items, web.TicketInfo{ID: "unrelated", Status: "open"})
						running = 1
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"tickets": items, "running_agents": running})
				case "/api/tickets/subject":
					details.Add(1)
					_ = json.NewEncoder(w).Encode(current())
				case "/api/events":
					first := opens.Add(1) == 1
					if !first {
						select {
						case <-allowReconnect:
						case <-r.Context().Done():
							return
						}
					}
					events, unsubscribe := broker.Subscribe()
					defer unsubscribe()
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					if first {
						defer close(returned)
						select {
						case <-gate:
						case <-r.Context().Done():
							return
						}
						if mode == "network" {
							return
						}
					}
					for {
						select {
						case event, ok := <-events:
							if !ok {
								return
							}
							data, _ := json.Marshal(event.Ticket)
							_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
							w.(http.Flusher).Flush()
						case <-r.Context().Done():
							return
						}
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			src := &apiSource{client: remote.New(srv.URL, "")}
			m := newModel(src)
			m.list.setTickets([]web.TicketInfo{seed}, 1)
			m, _ = m.openDetail(seed)
			events := src.Subscribe(ctx)
			event := receiveReconcileEvent(t, events)
			require.Equal(t, remote.SSEConnected, event.Type)
			runReconcileCmd(&m, applyReconcileMsg(&m, taskUpdatedMsg(event)))
			if mode == "overflow" {
				for i := range 65 {
					broker.Broadcast(web.TicketEvent{Type: "ticket_updated", Ticket: web.TicketInfo{ID: "unrelated", Status: "open", Attempt: i}})
				}
			}
			close(gate)
			if mode == "overflow" {
				for range 64 {
					event = receiveReconcileEvent(t, events)
					require.Equal(t, "unrelated", event.Ticket.ID)
					runReconcileCmd(&m, applyReconcileMsg(&m, taskUpdatedMsg(event)))
				}
			}
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Fatal("first stream did not terminate")
			}
			// The subject changes after the old stream drains, not in the dropped event.
			changed.Store(true)
			broker.Broadcast(web.TicketEvent{Type: "ticket_updated", Ticket: current()})
			close(allowReconnect)
			event = receiveReconcileEvent(t, events)
			require.Equal(t, remote.SSEConnected, event.Type)
			runReconcileCmd(&m, applyReconcileMsg(&m, taskUpdatedMsg(event)))
			require.True(t, m.list.connected)
			require.Equal(t, viewDetail, m.view)
			require.Equal(t, "done", m.detail.ticket.Status)
			require.Equal(t, "gap body", m.detail.ticket.Body)
			statuses := make(map[string]string)
			for _, info := range m.list.tickets {
				statuses[info.ID] = info.Status
			}
			require.Equal(t, map[string]string{"subject": "done", "gap-added": "open"}, statuses)
			require.Zero(t, m.list.running)
			require.Equal(t, int32(2), lists.Load())
			require.Equal(t, int32(2), details.Load())
		})
	}
}

func TestReconnectSnapshotPreservesEventsDuringDecode(t *testing.T) {
	seed := web.TicketInfo{ID: "subject", Status: "in_progress", Body: "old body", Kontora: true}
	gone := web.TicketInfo{ID: "deleted", Status: "open"}
	archived := web.TicketInfo{ID: "archived", Status: "open"}
	added := web.TicketInfo{ID: "added", Status: "open"}
	var lists, details atomic.Int32
	listStarted, detailStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload any
		var started chan struct{}
		switch r.URL.Path {
		case "/api/tickets":
			lists.Add(1)
			payload = map[string]any{"tickets": []web.TicketInfo{seed, gone, archived}, "running_agents": 1}
			started = listStarted
		case "/api/tickets/subject":
			if details.Add(1) > 1 {
				info := seed
				info.Status, info.Body = "human_review", "new body"
				_ = json.NewEncoder(w).Encode(info)
				return
			}
			payload, started = seed, detailStarted
		default:
			http.NotFound(w, r)
			return
		}
		data, _ := json.Marshal(payload)
		// Headers and partial JSON arrive before the newer events.
		_, _ = w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-release:
			_, _ = w.Write(data[len(data)/2:])
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)
	m := newModel(&apiSource{client: remote.New(srv.URL, "")})
	m.list.setTickets([]web.TicketInfo{seed, gone, archived}, 1)
	m, _ = m.openDetail(seed)
	cmd := applyReconcileMsg(&m, taskUpdatedMsg{Type: remote.SSEConnected})
	batch := cmd().(tea.BatchMsg)
	require.Len(t, batch, 2)
	responses := make(chan tea.Msg, 2)
	for _, child := range batch {
		go func() { responses <- child() }()
	}
	for _, started := range []chan struct{}{listStarted, detailStarted} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("snapshot did not start")
		}
	}
	newer := seed
	newer.Status, newer.Body = "human_review", ""
	detailCmd := applyReconcileMsg(&m, taskUpdatedMsg{Type: "ticket_updated", Ticket: newer})
	applyReconcileMsg(&m, taskUpdatedMsg{Type: "ticket_deleted", Ticket: gone})
	archived.Status = "archived"
	applyReconcileMsg(&m, taskUpdatedMsg{Type: "ticket_updated", Ticket: archived})
	applyReconcileMsg(&m, taskUpdatedMsg{Type: "ticket_updated", Ticket: added})
	secondary := seed
	secondary.Source, secondary.Status = "other", "done"
	require.Nil(t, applyReconcileMsg(&m, taskUpdatedMsg{Type: "secondary_ticket_updated", Ticket: secondary}))
	runReconcileCmd(&m, detailCmd)
	unblock()
	for range 2 {
		select {
		case response := <-responses:
			runReconcileCmd(&m, applyReconcileMsg(&m, response))
		case <-time.After(time.Second):
			t.Fatal("snapshot did not complete")
		}
	}
	require.Equal(t, viewDetail, m.view)
	require.Equal(t, "human_review", m.detail.ticket.Status)
	require.Equal(t, "new body", m.detail.ticket.Body)
	statuses := make(map[string]string)
	for _, info := range m.list.tickets {
		statuses[info.ID] = info.Status
	}
	require.Equal(t, map[string]string{"subject": "human_review", "added": "open"}, statuses)
	require.Zero(t, m.list.running)
	require.Equal(t, int32(1), lists.Load())
	require.Equal(t, int32(2), details.Load())
	require.Nil(t, m.listTouched, "event protection is retained only during a snapshot")
}

func TestReconnectDiscardsPreConnectionList(t *testing.T) {
	for _, previous := range []string{"absent", "stale", "failure"} {
		for _, detailFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/detail first=%t", previous, detailFirst), func(t *testing.T) {
				seed := web.TicketInfo{ID: "subject", Title: "Before", Status: "in_progress", Body: "old body", Kontora: true}
				restored := seed
				restored.Title, restored.Status, restored.Body = "Restored", "done", "restored body"
				var lists atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/tickets":
						rows := []web.TicketInfo{restored}
						if lists.Add(1) == 1 {
							switch previous {
							case "absent":
								rows = nil
							case "stale":
								stale := seed
								stale.Title = "Stale"
								rows = []web.TicketInfo{stale}
							case "failure":
								w.WriteHeader(http.StatusServiceUnavailable)
								return
							}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"tickets": rows})
					case "/api/tickets/subject":
						_ = json.NewEncoder(w).Encode(restored)
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				m := newModel(&apiSource{client: remote.New(srv.URL, "")})
				m.list.setTickets([]web.TicketInfo{seed}, 1)
				m, _ = m.openDetail(seed)
				request := applyReconcileMsg(&m, fetchTicketsMsg{})
				old := request()
				detail := applyReconcileMsg(&m, taskUpdatedMsg{Type: remote.SSEConnected})()
				if detailFirst {
					applyReconcileMsg(&m, detail)
				}
				trailing := applyReconcileMsg(&m, old)
				require.Equal(t, viewDetail, m.view, "a pre-connection absence cannot close detail")
				require.Equal(t, []web.TicketInfo{seed}, m.list.tickets, "obsolete rows must not reach the board")
				require.NoError(t, m.list.err, "an obsolete failure cannot replace current recovery")
				require.NotNil(t, trailing, "reconnection still needs a fresh authoritative list")
				if !detailFirst {
					applyReconcileMsg(&m, detail)
				}
				require.Equal(t, restored, m.detail.ticket)
				runReconcileCmd(&m, trailing)
				require.Equal(t, viewDetail, m.view)
				require.Equal(t, restored, m.detail.ticket)
				require.Equal(t, []web.TicketInfo{restored}, m.list.tickets)
				require.Equal(t, int32(2), lists.Load())
				require.False(t, m.listFlight)
				require.False(t, m.listAgain)
			})
		}
	}
}

func TestReconnectCoalescesListsAndRetriesFailures(t *testing.T) {
	seed := web.TicketInfo{ID: "subject", Status: "open", Body: "body"}
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(failFirst), func(t *testing.T) {
			var lists atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/tickets" {
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if lists.Add(1) == 1 && failFirst {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"tickets": []web.TicketInfo{seed}})
			}))
			defer srv.Close()
			m := newModel(&apiSource{client: remote.New(srv.URL, "")})
			first := applyReconcileMsg(&m, taskUpdatedMsg{Type: remote.SSEConnected})
			for range 20 {
				require.Nil(t, applyReconcileMsg(&m, taskUpdatedMsg{Type: remote.SSEConnected}))
			}
			runReconcileCmd(&m, first)
			require.Equal(t, int32(2), lists.Load(), "overlapping recovery requests need one trailing list")
			require.False(t, m.listFlight)
			require.False(t, m.listRetry)
			require.NoError(t, m.list.err)
			require.Equal(t, []web.TicketInfo{seed}, m.list.tickets)
		})
	}
}

func TestReconnectDetailSelectionAndFailure(t *testing.T) {
	seed := web.TicketInfo{ID: "subject", Status: "open", Body: "body"}
	for _, mode := range []string{"closed", "different", "deleted", "archived", "failure"} {
		t.Run(mode, func(t *testing.T) {
			m := newModel(&mockSource{connected: true, detail: seed})
			m.list.setTickets([]web.TicketInfo{seed}, 0)
			m, _ = m.openDetail(seed)
			cmd := applyReconcileMsg(&m, taskUpdatedMsg{Type: remote.SSEConnected})
			batch := cmd().(tea.BatchMsg)
			response := batch[1]().(taskRefreshedMsg)
			switch mode {
			case "closed":
				applyReconcileMsg(&m, tea.KeyMsg{Type: tea.KeyEscape})
			case "different":
				m, _ = m.openDetail(web.TicketInfo{ID: "other", Body: "other body"})
			case "deleted":
				applyReconcileMsg(&m, ticketsRefreshedMsg{})
			case "archived":
				response.info.Status = "archived"
			case "failure":
				response.err = fmt.Errorf("temporary detail failure")
			}
			applyReconcileMsg(&m, response)
			switch mode {
			case "different":
				require.Equal(t, "other", m.detail.ticket.ID)
				require.Equal(t, "other body", m.detail.ticket.Body)
			case "failure":
				require.Equal(t, viewDetail, m.view)
				require.Equal(t, seed, m.detail.ticket)
				require.True(t, m.detailRetry)
				require.Error(t, m.detail.err)
				applyReconcileMsg(&m, taskRefreshedMsg{seq: m.refreshSeq - 1, err: fmt.Errorf("old error")})
				require.EqualError(t, m.detail.err, "temporary detail failure")
			default:
				require.Equal(t, viewList, m.view)
			}
		})
	}
}
