package web

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty/v2"
	"github.com/worksonmyai/kontora/internal/automation"
	"github.com/worksonmyai/kontora/internal/tmux"
)

type AutomationTerminalTarget struct {
	Session  string              `json:"session"`
	Window   string              `json:"window"`
	Identity tmux.WindowIdentity `json:"identity"`
}

type AutomationTerminalService interface {
	AutomationTerminal(id string, n uint64) (AutomationTerminalTarget, error)
}

func (s *Server) handleAutomationTerminal(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.svc.(AutomationTerminalService)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "automation terminals unavailable")
		return
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil || n == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid run number")
		return
	}
	target, err := svc.AutomationTerminal(r.PathValue("id"), n)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, target)
}

// A viewer has one link to the authorized window. Its temporary placeholder is
// a bounded sleep process, never the user's shell. No window index is assumed.
type automationViewer struct {
	target  AutomationTerminalTarget
	name    string
	session string
	index   string
}

func automationTmuxQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// The server-side check prevents a socket reconnect from acting on IDs reused
// by another server. Cleanup omits the window target because it may be gone.
func (v *automationViewer) command(ctx context.Context, needsWindow bool, args ...string) *exec.Cmd {
	id := v.target.Identity
	guard := fmt.Sprintf("#{&&:#{==:#{pid},%d},#{==:#{start_time},%d}}", id.ServerPID, id.ServerStartedAt)
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = automationTmuxQuote(arg)
	}
	command := []string{"-S", id.SocketPath, "-N", "-u", "if-shell"}
	if needsWindow {
		command = append(command, "-t", id.WindowID)
	}
	command = append(command, "-F", guard, strings.Join(quoted, " "), "display-message -p kontora-server-identity-changed")
	cmd := exec.CommandContext(ctx, "tmux", command...)
	cmd.Env = append(os.Environ(), "TMUX=", "TERM=xterm-256color", "LANG=en_US.UTF-8")
	return cmd
}

func (v *automationViewer) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := v.command(ctx, true, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("preparing automation viewer: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func (v *automationViewer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := v.session
	if target == "" {
		target = "=" + v.name
	}
	_ = v.command(ctx, false, "kill-session", "-t", target).Run()
}

func createAutomationViewer(ctx context.Context, target AutomationTerminalTarget, cols, rows uint16) (_ *automationViewer, err error) {
	if present, err := tmux.InspectWindowIdentity(target.Identity); err != nil || !present {
		return nil, fmt.Errorf("automation window is unavailable: %v", err)
	}
	v := &automationViewer{target: target, name: fmt.Sprintf("kontora-auto-view-%x", rand.Uint64())}
	defer func() {
		if err != nil {
			v.close()
		}
	}()
	out, err := v.run(ctx, "new-session", "-d", "-P", "-F", "#{session_id}\t#{window_index}", "-s", v.name,
		"-x", strconv.Itoa(int(cols)), "-y", strconv.Itoa(int(rows)), "--", "/bin/sleep", "300")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\t")
	if len(fields) != 2 {
		return nil, fmt.Errorf("invalid automation viewer identity")
	}
	sessionNumber, sessionErr := strconv.ParseUint(strings.TrimPrefix(fields[0], "$"), 10, 64)
	index, indexErr := strconv.ParseUint(fields[1], 10, 32)
	if sessionErr != nil || fields[0] != "$"+strconv.FormatUint(sessionNumber, 10) || indexErr != nil {
		return nil, fmt.Errorf("invalid automation viewer identity")
	}
	v.session, v.index = fields[0], strconv.FormatUint(index, 10)
	// Replace the actual placeholder index, including a nonzero base-index.
	if out, err = v.run(ctx, "link-window", "-k", "-s", target.Identity.WindowID, "-t", v.session+":"+v.index); err != nil || out != "" {
		return nil, fmt.Errorf("cannot link automation window: %s: %v", out, err)
	}
	linked, err := v.run(ctx, "display-message", "-p", "-t", v.session+":"+v.index, "#{window_id}")
	if err != nil || linked != target.Identity.WindowID {
		return nil, fmt.Errorf("automation viewer target changed")
	}
	if present, err := tmux.InspectWindowIdentity(target.Identity); err != nil || !present {
		return nil, fmt.Errorf("automation window ended during viewer creation")
	}
	return v, nil
}

func automationTerminalMatches(svc AutomationTerminalService, id string, n uint64, target AutomationTerminalTarget) bool {
	current, err := svc.AutomationTerminal(id, n)
	if err != nil || current != target {
		return false
	}
	present, err := tmux.InspectWindowIdentity(target.Identity)
	return err == nil && present
}

func (s *Server) handleAutomationTerminalWS(w http.ResponseWriter, r *http.Request) {
	svc, ok := s.svc.(AutomationTerminalService)
	if !ok {
		http.Error(w, "automation terminals unavailable", http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	if err != nil || n == 0 {
		http.Error(w, "invalid run number", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	target, err := svc.AutomationTerminal(id, n)
	if err != nil {
		writeAutomationError(w, err)
		return
	}
	present, err := tmux.InspectWindowIdentity(target.Identity)
	if err != nil {
		writeAutomationError(w, fmt.Errorf("%w: terminal identity cannot be verified", automation.ErrUnavailable))
		return
	}
	if !present {
		http.Error(w, "no terminal session", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.wsPatterns})
	if err != nil {
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	cols, rows := uint16(80), uint16(24)
	if value, err := strconv.Atoi(r.URL.Query().Get("cols")); err == nil && value > 0 {
		cols = clampDim(value)
	}
	if value, err := strconv.Atoi(r.URL.Query().Get("rows")); err == nil && value > 0 {
		rows = clampDim(value)
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if !automationTerminalMatches(svc, id, n, target) {
		conn.Close(websocket.StatusNormalClosure, "run ended")
		return
	}
	view, err := createAutomationViewer(ctx, target, cols, rows)
	if err != nil {
		conn.Close(websocket.StatusInternalError, "cannot create viewer")
		return
	}
	defer view.close()
	if !automationTerminalMatches(svc, id, n, target) {
		conn.Close(websocket.StatusNormalClosure, "run ended")
		return
	}
	rw := r.URL.Query().Get("rw") == "1"
	args := []string{"attach-session", "-t", view.session + ":" + view.index}
	if !rw {
		args = append(args, "-r")
	}
	cmd := view.command(ctx, true, args...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		conn.Close(websocket.StatusInternalError, "cannot start terminal")
		return
	}
	defer func() {
		cancel()
		_ = ptmx.Close()
		_ = cmd.Wait()
	}()
	go func() {
		defer cancel()
		s.readClientMessages(ctx, conn, ptmx, view.name, target.Identity.SocketPath, rw)
	}()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !automationTerminalMatches(svc, id, n, target) {
					cancel()
					return
				}
			}
		}
	}()
	s.pipeOutput(ctx, conn, ptmx, id, wsWriteTimeout)
}
