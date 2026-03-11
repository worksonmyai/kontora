package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"

	"github.com/coder/websocket"
	"github.com/creack/pty/v2"

	"github.com/worksonmyai/kontora/internal/tmux"
)

type clientMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Data string `json:"data,omitempty"`
}

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
		cols = c
	}
	if ro, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && ro > 0 {
		rows = ro
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	rw := r.URL.Query().Get("rw") == "1"

	target := tmux.WindowTarget(tmux.DefaultSessionName, id)
	args := []string{"attach-session", "-t", target}
	if !rw {
		args = append(args, "-r")
	}
	cmd := exec.CommandContext(ctx, "tmux", args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

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
		s.readClientMessages(ctx, conn, ptmx)
	}()

	s.log.Info("terminal session connected", "ticket", id)
	s.pipeOutput(ctx, conn, ptmx, id)
	s.log.Info("terminal session disconnected", "ticket", id)
}

func (s *Server) readClientMessages(ctx context.Context, conn *websocket.Conn, ptmx *os.File) {
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
					Rows: uint16(msg.Rows),
					Cols: uint16(msg.Cols),
				})
			}
		case "input":
			if msg.Data != "" {
				_, _ = ptmx.WriteString(msg.Data)
			}
		}
	}
}

func (s *Server) pipeOutput(ctx context.Context, conn *websocket.Conn, r io.Reader, taskID string) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if writeErr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); writeErr != nil {
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
