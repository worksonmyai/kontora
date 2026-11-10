package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/worksonmyai/kontora/internal/tmux"
)

const paneHistoryDepth = 5000

// The stdout reader must keep draining even when the browser stops reading:
// tmux can otherwise stop reading the agent's pane as well.
type paneQueue struct {
	mu      sync.Mutex
	packets []panePacket
	bytes   int
	ended   bool
	ready   chan struct{}
}

type panePacket struct {
	reset      bool
	cols, rows int
	data       []byte
}

func newPaneQueue() *paneQueue { return &paneQueue{ready: make(chan struct{}, 1)} }

func (q *paneQueue) signal() {
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *paneQueue) push(p panePacket) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.packets) >= 256 || q.bytes+len(p.data) > 2*1024*1024 {
		q.packets = nil
		q.bytes = 0
		return false
	}
	q.packets = append(q.packets, p)
	q.bytes += len(p.data)
	q.signal()
	return true
}

func (q *paneQueue) snapshot(s tmux.Snapshot) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.packets = []panePacket{{reset: true, cols: s.Cols, rows: s.Rows, data: s.Data}}
	q.bytes = len(s.Data)
	q.signal()
}

func (q *paneQueue) finish() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.ended = true
	q.signal()
}

func (q *paneQueue) pop(ctx context.Context) (panePacket, bool) {
	for {
		q.mu.Lock()
		if len(q.packets) > 0 {
			p := q.packets[0]
			q.packets[0] = panePacket{}
			q.packets = q.packets[1:]
			q.bytes -= len(p.data)
			q.mu.Unlock()
			return p, true
		}
		if q.ended {
			q.mu.Unlock()
			return panePacket{}, false
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return panePacket{}, false
		case <-q.ready:
		}
	}
}

type paneCommand struct {
	line    string
	replies int
	done    func([][]byte) error
}

type panePending struct {
	command paneCommand
	data    [][]byte
}

type paneStream struct {
	control      *tmux.Control
	queue        *paneQueue
	commands     chan paneCommand
	mu           sync.Mutex
	pending      []*panePending
	capturing    bool
	checkingSize bool
	cols, rows   int
}

func (p *paneStream) command(ctx context.Context, c paneCommand) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.commands <- c:
		return nil
	}
}

// Reader-generated commands cannot wait for stdin. A full command queue ends
// this viewer instead of propagating backpressure to the pane.
func (p *paneStream) schedule(c paneCommand) error {
	select {
	case p.commands <- c:
		return nil
	default:
		return fmt.Errorf("terminal command queue full")
	}
}

func (p *paneStream) writeCommands(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c := <-p.commands:
			p.mu.Lock()
			p.pending = append(p.pending, &panePending{command: c})
			p.mu.Unlock()
			if err := p.control.Send(c.line); err != nil {
				return err
			}
		}
	}
}

func (p *paneStream) snapshot() error {
	if p.capturing {
		return nil
	}
	p.capturing = true
	return p.schedule(paneCommand{line: p.control.SnapshotCommand(paneHistoryDepth), replies: 5, done: func(data [][]byte) error {
		s, err := tmux.BuildSnapshot(data)
		if err != nil {
			return err
		}
		p.cols, p.rows = s.Cols, s.Rows
		p.queue.snapshot(s)
		p.capturing = false
		return nil
	}})
}

func (p *paneStream) checkSize() error {
	if p.checkingSize {
		return nil
	}
	p.checkingSize = true
	return p.schedule(paneCommand{line: p.control.SizeCommand(), replies: 1, done: func(data [][]byte) error {
		p.checkingSize = false
		fields := strings.Fields(string(data[0]))
		if len(fields) != 2 {
			return fmt.Errorf("invalid pane size: %q", data[0])
		}
		cols, err := strconv.Atoi(fields[0])
		if err != nil {
			return err
		}
		rows, err := strconv.Atoi(fields[1])
		if err != nil {
			return err
		}
		if cols != p.cols || rows != p.rows {
			return p.snapshot()
		}
		return nil
	}})
}

func (p *paneStream) readOutput() error {
	for {
		event, err := p.control.ReadEvent()
		if err != nil {
			return err
		}
		switch event.Kind {
		case "exit":
			return nil
		case "output":
			if event.PaneID == p.control.PaneID && !p.capturing {
				if !p.queue.push(panePacket{data: event.Data}) {
					if err := p.snapshot(); err != nil {
						return err
					}
				}
			}
		case "layout":
			if event.WindowID == p.control.WindowID {
				if err := p.checkSize(); err != nil {
					return err
				}
			}
		case "reply":
			if event.Failed {
				return fmt.Errorf("tmux control command failed: %s", event.Data)
			}
			p.mu.Lock()
			if len(p.pending) == 0 {
				p.mu.Unlock()
				return fmt.Errorf("unexpected tmux reply")
			}
			pending := p.pending[0]
			pending.data = append(pending.data, event.Data)
			complete := len(pending.data) == pending.command.replies
			if complete {
				p.pending = p.pending[1:]
			}
			p.mu.Unlock()
			if complete && pending.command.done != nil {
				if err := pending.command.done(pending.data); err != nil {
					return err
				}
			}
		}
	}
}

func writePanePackets(ctx context.Context, conn binaryWriter, q *paneQueue) error {
	write := func(typ websocket.MessageType, data []byte) error {
		writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
		defer cancel()
		return conn.Write(writeCtx, typ, data)
	}
	for {
		p, ok := q.pop(ctx)
		if !ok {
			return ctx.Err()
		}
		if p.reset {
			data, err := json.Marshal(clientMsg{Type: "reset", Cols: p.cols, Rows: p.rows})
			if err != nil {
				return err
			}
			if err := write(websocket.MessageText, data); err != nil {
				return err
			}
		}
		for len(p.data) > 0 {
			n := min(len(p.data), ptyReadBufSize)
			if err := write(websocket.MessageBinary, p.data[:n]); err != nil {
				return err
			}
			p.data = p.data[n:]
		}
	}
}

func (s *Server) handlePaneTerminal(ctx context.Context, conn *websocket.Conn, id string, cols, rows int, rw bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	viewer := fmt.Sprintf("kontora-view-%x-%x", rand.Uint64(), rand.Uint64())
	control, err := tmux.StartControl(ctx, s.tmuxSession, id, viewer, rw)
	if err != nil {
		s.log.Error("pane terminal setup failed", "ticket", id, "err", err)
		conn.Close(websocket.StatusInternalError, "failed to start terminal")
		return
	}
	p := &paneStream{control: control, queue: newPaneQueue(), commands: make(chan paneCommand, 64), capturing: true}
	// Resize before capturing so the first reset reports the pane's resulting
	// dimensions rather than its detached size.
	p.commands <- paneCommand{line: control.ResizeCommand(cols, rows), replies: 2, done: func(_ [][]byte) error {
		p.capturing = false
		return p.snapshot()
	}}
	var wg sync.WaitGroup
	commandCtx, stopCommands := context.WithCancel(ctx)
	defer stopCommands()
	wg.Go(func() {
		defer stopCommands()
		// Broken stdin can precede %exit. Stdout decides whether output can drain.
		if err := p.writeCommands(commandCtx); err != nil && commandCtx.Err() == nil {
			s.log.Debug("pane terminal commands ended", "ticket", id, "err", err)
		}
	})
	wg.Go(func() {
		defer stopCommands()
		if err := p.readOutput(); err != nil {
			if ctx.Err() == nil {
				s.log.Debug("pane terminal stream ended", "ticket", id, "err", err)
			}
			cancel()
			return
		}
		// %exit follows the final pane output, which may still be queued.
		p.queue.finish()
	})
	wg.Go(func() {
		defer cancel()
		if err := writePanePackets(ctx, conn, p.queue); err == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		} else if ctx.Err() == nil {
			s.log.Debug("pane terminal stream ended", "ticket", id, "err", err)
		}
	})
	s.log.Info("terminal session connected", "ticket", id, "view_session", viewer, "rw", rw, "stream", "pane")
	defer func() {
		cancel()
		_ = control.Close()
		wg.Wait()
		s.log.Info("terminal session disconnected", "ticket", id, "view_session", viewer, "rw", rw, "stream", "pane")
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg clientMsg
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "resize":
			if msg.Cols > 0 && msg.Rows > 0 {
				if p.command(commandCtx, paneCommand{line: control.ResizeCommand(int(clampDim(msg.Cols)), int(clampDim(msg.Rows))), replies: 2}) != nil && ctx.Err() != nil {
					return
				}
			}
		case "input":
			if !rw {
				continue
			}
			data := []byte(msg.Data)
			for len(data) > 0 {
				n := min(len(data), 256)
				var line strings.Builder
				fmt.Fprintf(&line, "send-keys -H -t %s", control.PaneID)
				for _, b := range data[:n] {
					fmt.Fprintf(&line, " %02x", b)
				}
				if p.command(commandCtx, paneCommand{line: line.String(), replies: 1}) != nil {
					if ctx.Err() != nil {
						return
					}
					break
				}
				data = data[n:]
			}
		}
	}
}
