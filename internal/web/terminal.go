package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty/v2"

	"github.com/worksonmyai/kontora/internal/tmux"
)

const (
	// A blocking pty read returns whatever is already queued, so a buffer this
	// size turns a burst into a few WebSocket frames instead of dozens without
	// making a lone keystroke wait for more bytes.
	ptyReadBufSize = 32 * 1024

	// Without a per-write bound, a browser tab that stops reading blocks the pty
	// read loop. That backpressure reaches the tmux server and slows the pane for
	// every viewer, including a CLI user attached to the main session.
	wsWriteTimeout = 10 * time.Second

	// Upper bound on browser-requested geometry. pty.Winsize stores dimensions as
	// uint16, so an unclamped 65536 truncates to 0. At the UI's 13px JetBrains
	// Mono, 1000 columns needs a viewport near 7800px, past any real display.
	maxTerminalDim = 1000
)

type clientMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Data string `json:"data,omitempty"`
}

// Callers reject non-positive dimensions first, so only the upper end is capped.
func clampDim(v int) int { return min(v, maxTerminalDim) }

func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.svc.HasTerminalSession(id) {
		http.Error(w, "no terminal session", http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Error("websocket accept failed", "err", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	cols, rows := 80, 24
	if c, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && c > 0 {
		cols = clampDim(c)
	}
	if ro, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && ro > 0 {
		rows = clampDim(ro)
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	rw := r.URL.Query().Get("rw") == "1"

	// Create a linked tmux session so the web viewer gets independent sizing
	// without shrinking the pane in the main session.
	viewSession := fmt.Sprintf("kontora-view-%s-%x", id, rand.Uint32())
	mainSession := "=" + tmux.DefaultSessionName
	newCmd := exec.Command("tmux", "new-session", "-d", "-t", mainSession, "-s", viewSession,
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	newCmd.Env = append(os.Environ(), "LANG=en_US.UTF-8")
	out, err := newCmd.CombinedOutput()
	if err != nil {
		s.log.Error("linked session create failed", "err", err, "output", string(out), "ticket", id)
		conn.Close(websocket.StatusInternalError, "failed to create viewer session")
		return
	}
	defer func() { _ = exec.Command("tmux", "kill-session", "-t", "="+viewSession).Run() }()

	// tmux resolves a window-qualified attach target and makes that window
	// current, so the ticket's window needs no separate select-window spawn.
	args := []string{"attach-session", "-t", tmux.WindowTarget(viewSession, id)}
	if !rw {
		args = append(args, "-r")
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "LANG=en_US.UTF-8")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Rows: uint16(rows),
		Cols: uint16(cols),
	})
	if err != nil {
		s.log.Error("pty start failed", "err", err, "ticket", id)
		conn.Close(websocket.StatusInternalError, "failed to start terminal")
		return
	}
	defer ptmx.Close()

	go func() {
		defer cancel()
		s.readClientMessages(ctx, conn, ptmx, viewSession)
	}()

	s.log.Info("terminal session connected", "ticket", id, "view_session", viewSession)
	s.pipeOutput(ctx, conn, ptmx, id, wsWriteTimeout)
	s.log.Info("terminal session disconnected", "ticket", id, "view_session", viewSession)
}

func (s *Server) readClientMessages(ctx context.Context, conn *websocket.Conn, ptmx *os.File, viewSession string) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg clientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{
					Rows: uint16(clampDim(msg.Rows)),
					Cols: uint16(clampDim(msg.Cols)),
				})
				// Force tmux to redraw after PTY resize to prevent
				// rendering artifacts from stale cursor positions.
				_ = exec.Command("tmux", "refresh-client", "-t", "="+viewSession).Run()
			}
		case "input":
			if msg.Data != "" {
				_, _ = ptmx.WriteString(msg.Data)
			}
		}
	}
}

// binaryWriter is the part of *websocket.Conn that pipeOutput uses, so a test
// can supply a writer that stalls.
type binaryWriter interface {
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
}

func (s *Server) pipeOutput(ctx context.Context, conn binaryWriter, r io.Reader, taskID string, writeTimeout time.Duration) {
	buf := make([]byte, ptyReadBufSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			writeErr := conn.Write(writeCtx, websocket.MessageBinary, buf[:n])
			cancel()
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			if err != io.EOF {
				s.log.Debug("pty read error", "err", err, "ticket", taskID)
			}
			return
		}
	}
}
