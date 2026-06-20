package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/coder/websocket"
	"golang.org/x/term"
)

// termMsg is the JSON frame the daemon's terminal endpoint expects for input
// and resize events. It mirrors the protocol in internal/web/terminal.go.
type termMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Data string `json:"data,omitempty"`
}

// Attach connects to a running ticket's terminal over the WebSocket endpoint
// and bridges the local TTY bidirectionally. The Bearer token (if any) is sent
// on the handshake. Keystrokes go up as input frames; the agent's output comes
// down as binary frames and is written to stdout. SIGWINCH triggers a resize
// frame. It returns when the connection closes.
func Attach(ctx context.Context, c *Client, id string, rw bool) error {
	fd := int(os.Stdin.Fd())
	isTTY := term.IsTerminal(fd)

	cols, rows := 80, 24
	if isTTY {
		if w, h, err := term.GetSize(fd); err == nil && w > 0 && h > 0 {
			cols, rows = w, h
		}
	}

	endpoint, err := wsURL(c.base, id, rw, cols, rows)
	if err != nil {
		return err
	}

	opts := &websocket.DialOptions{}
	if c.token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + c.token}}
	}

	conn, resp, err := websocket.Dial(ctx, endpoint, opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("connecting to remote terminal: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(1 << 20)

	if isTTY {
		old, err := term.MakeRaw(fd)
		if err == nil {
			defer func() { _ = term.Restore(fd, old) }()
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex

	if isTTY {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-winch:
					if w, h, err := term.GetSize(fd); err == nil {
						_ = writeFrame(ctx, conn, &writeMu, termMsg{Type: "resize", Cols: w, Rows: h})
					}
				}
			}
		}()
	}

	return runBridge(ctx, conn, os.Stdin, os.Stdout, &writeMu)
}

// runBridge copies input -> JSON input frames and binary output frames -> out.
// It returns when the connection closes or an output write fails. Factored out
// of Attach so the framing can be tested without a real TTY.
func runBridge(ctx context.Context, conn *websocket.Conn, in io.Reader, out io.Writer, writeMu *sync.Mutex) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				if werr := writeFrame(ctx, conn, writeMu, termMsg{Type: "input", Data: string(buf[:n])}); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			// A normal/going-away close or a cancelled context is a clean end of
			// session. Anything else (abnormal close, transport reset, read limit
			// exceeded) is surfaced so the user sees why the session dropped.
			status := websocket.CloseStatus(err)
			if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if _, werr := out.Write(data); werr != nil {
			return werr
		}
	}
}

func writeFrame(ctx context.Context, conn *websocket.Conn, mu *sync.Mutex, m termMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	return conn.Write(ctx, websocket.MessageText, b)
}

// wsURL builds the terminal WebSocket URL, converting http(s) to ws(s) and
// appending the ticket path and rw/cols/rows query parameters.
func wsURL(base, id string, rw bool, cols, rows int) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid daemon URL %q: %w", base, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already a websocket scheme
	default:
		return "", fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/terminal/" + id

	q := u.Query()
	rwVal := "0"
	if rw {
		rwVal = "1"
	}
	q.Set("rw", rwVal)
	q.Set("cols", strconv.Itoa(cols))
	q.Set("rows", strconv.Itoa(rows))
	u.RawQuery = q.Encode()

	return u.String(), nil
}
