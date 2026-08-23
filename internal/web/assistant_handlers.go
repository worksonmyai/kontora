package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// assistantTextMax bounds one composer message. It is generous enough for a
// pasted stack trace and short enough that the argv the agent is spawned with
// stays under the per-string limit.
const assistantTextMax = 32 << 10

// assistantContextMax bounds the page context a message carries. The pane sends
// a handful of lines about the open view, so anything larger is a client that
// went wrong rather than a page worth describing.
const assistantContextMax = 2 << 10

func (s *Server) handleAssistantConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.AssistantConfig())
}

func (s *Server) handleListAssistantThreads(w http.ResponseWriter, _ *http.Request) {
	threads, err := s.svc.ListAssistantThreads()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if threads == nil {
		threads = []AssistantThreadInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (s *Server) handleCreateAssistantThread(w http.ResponseWriter, r *http.Request) {
	var req CreateAssistantThreadRequest
	// An empty body is a thread on the configured defaults, which is what the
	// pane's own "new chat" sends.
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	thread, err := s.svc.CreateAssistantThread(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, thread)
}

func (s *Server) handleGetAssistantThread(w http.ResponseWriter, r *http.Request) {
	thread, err := s.svc.GetAssistantThread(r.PathValue("id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, thread)
}

func (s *Server) handleDeleteAssistantThread(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteAssistantThread(r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssistantActivity(w http.ResponseWriter, r *http.Request) {
	after := 0
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSONError(w, http.StatusBadRequest, "after must be a non-negative integer")
			return
		}
		after = n
	}
	info, err := s.svc.AssistantActivity(AssistantActivityQuery{
		ID:          r.PathValue("id"),
		After:       after,
		IfNoneMatch: r.Header.Get("If-None-Match"),
	})
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if info.ETag != "" {
		w.Header().Set("ETag", info.ETag)
	}
	if info.NotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleAssistantMessage(w http.ResponseWriter, r *http.Request) {
	var req AssistantMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Text == "" {
		writeJSONError(w, http.StatusBadRequest, "text is required")
		return
	}
	if len(req.Text) > assistantTextMax {
		writeJSONError(w, http.StatusBadRequest, "text is too long")
		return
	}
	if len(req.Context) > assistantContextMax {
		writeJSONError(w, http.StatusBadRequest, "context is too long")
		return
	}
	if err := s.svc.PostAssistantMessage(r.PathValue("id"), req); err != nil {
		writeServiceError(w, err)
		return
	}
	// The reply arrives through the activity poll, not here.
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleAssistantStop(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.StopAssistantTurn(r.PathValue("id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssistantGate(w http.ResponseWriter, r *http.Request) {
	var req AssistantGateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch req.Decision {
	case "approve", "skip":
	default:
		writeJSONError(w, http.StatusBadRequest, `decision must be "approve" or "skip"`)
		return
	}
	if err := s.svc.ResolveAssistantGate(r.PathValue("gid"), req.Decision == "approve"); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAssistantGateAsk is the agent side of the gate. It blocks while a write
// waits for the person at the pane, so it has no timeout of its own: the gate's
// own deadline is what ends the wait.
func (s *Server) handleAssistantGateAsk(w http.ResponseWriter, r *http.Request) {
	var req AssistantGateAskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	resp, err := s.svc.AskAssistantGate(req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeOptionalJSON decodes a body that may be empty, which is what a POST
// with no options sends.
func decodeOptionalJSON(r *http.Request, out any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil && err.Error() != "EOF" {
		return err
	}
	return nil
}

// assistantStreamTick batches the deltas claude emits tens of times a second
// into one frame. Server-side, because a client cannot un-send a frame.
const assistantStreamTick = 100 * time.Millisecond

// assistantStreamKeepalive bounds the silence: a proxy drops an idle
// connection.
const assistantStreamKeepalive = 20 * time.Second

// handleAssistantStream pushes the message the agent is writing as it grows.
// Not the SSEBroker: that drops on a full channel, which silently corrupts a
// sentence. Each connection pulls a cumulative snapshot, which cannot drop.
func (s *Server) handleAssistantStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	first, err := s.svc.AssistantPartial(id)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !first.Running && first.Text == "" {
		// EventSource does not retry a 204, so a stale pane stops asking.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	send := func(event string, data any) {
		body, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
		flusher.Flush()
	}

	gen, sent, tool := first.Gen, first.Text, first.Tool
	sealed := first.Sealed
	send("reset", map[string]any{"gen": gen, "text": sent, "tool": tool})
	if sealed {
		send("end", struct{}{})
	}

	ticker := time.NewTicker(assistantStreamTick)
	defer ticker.Stop()
	quiet := time.Now()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}

		p, err := s.svc.AssistantPartial(id)
		if err != nil {
			// The thread was deleted under the stream; its poll will say so.
			return
		}

		switch {
		case p.Text == "" && sent != "":
			// The session file now carries it. Nothing is sent: the poll drops
			// the text in the same response that adds the settled row, and
			// blanking it here first would leave a frame showing neither.
		case p.Gen != gen:
			// A new block replaces rather than appends, so it goes whole.
			gen, sent, tool = p.Gen, p.Text, p.Tool
			send("reset", map[string]any{"gen": gen, "text": sent, "tool": tool})
			quiet = time.Now()
		case len(p.Text) > len(sent) && strings.HasPrefix(p.Text, sent):
			suffix := p.Text[len(sent):]
			sent = p.Text
			send("delta", map[string]any{"gen": gen, "text": suffix})
			quiet = time.Now()
		case p.Text != sent:
			// It moved in a way an append cannot describe. Resend, don't guess.
			sent, tool = p.Text, p.Tool
			send("reset", map[string]any{"gen": gen, "text": sent, "tool": tool})
			quiet = time.Now()
		}

		// An empty name is not sent: the same poll response clears that row.
		if p.Tool != tool && p.Tool != "" {
			tool = p.Tool
			send("tool", map[string]any{"gen": gen, "name": tool})
			quiet = time.Now()
		}
		if p.Sealed && !sealed {
			send("end", struct{}{})
			quiet = time.Now()
		}
		sealed = p.Sealed

		if !p.Running {
			send("done", struct{}{})
			return
		}
		if time.Since(quiet) >= assistantStreamKeepalive {
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
			quiet = time.Now()
		}
	}
}
