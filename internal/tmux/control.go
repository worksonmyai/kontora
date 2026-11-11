package tmux

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const controlTimeout = 5 * time.Second

// Reply Data uses LF separators without a final protocol LF. Layout Data is
// not pane geometry. Only stdin command replies (flags 1) are exposed.
type ControlEvent struct {
	Kind     string
	PaneID   string
	WindowID string
	Data     []byte
	Failed   bool
}

// Control requires one continuous ReadEvent reader, independent of Send and
// network writes. Otherwise tmux can stall the agent's pane.
// Close unblocks ReadEvent and Send.
// PaneID and WindowID are validated tmux IDs and must not be modified.
type Control struct {
	PaneID   string
	WindowID string

	viewer   string
	session  string
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	reader   *bufio.Reader
	done     chan struct{}
	sendMu   sync.Mutex
	once     sync.Once
	closeErr error
}

func controlNumericID(s string, prefix byte) bool {
	return len(s) > 1 && s[0] == prefix && decimal(s[1:])
}

func decimal(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func validViewer(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func controlRun(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-u"}, args...)...)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("tmux control setup/cleanup: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

// viewer must be a unique generated ASCII name, not a ticket ID.
// mainSession and windowName are passed only as setup arguments.
// Stdin commands use numeric IDs and the generated viewer name.
// Cancellation closes and reaps the running client.
func StartControl(ctx context.Context, mainSession, windowName, viewer string, rw bool) (*Control, error) {
	if mainSession == "" || windowName == "" || strings.ContainsAny(mainSession+windowName, "\x00\r\n") || !validViewer(viewer) {
		return nil, errors.New("tmux control: invalid session, window or generated viewer name")
	}
	setup, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	// list-panes has no after-hook and, unlike display-message, cannot succeed
	// with a missing target and an empty context. Refuse multi-pane windows.
	out, err := controlRun(setup, "list-panes", "-t", WindowTarget(mainSession, windowName), "-F", "#{pane_id} #{window_id}")
	if err != nil {
		return nil, err
	}
	ids := strings.Split(strings.TrimSuffix(string(out), "\n"), " ")
	if len(ids) != 2 || !controlNumericID(ids[0], '%') || !controlNumericID(ids[1], '@') {
		return nil, fmt.Errorf("tmux control: expected one pane with numeric IDs, got %q", out)
	}
	c := &Control{PaneID: ids[0], WindowID: ids[1], viewer: viewer}
	// tmux 3.8 has no per-command ignore-after-hooks flag. Empty session-local
	// hooks mask global/window/pane hooks without modifying the agent's options.
	// Both commands resolve their pane in the attached viewer's session.
	out, err = controlRun(setup,
		"new-session", "-d", "-P", "-F", "#{session_id}", "-s", viewer, "-n", "__init", "--", "sleep", "86400", ";",
		"set-option", "-t", "="+viewer+":", "detach-on-destroy", "on", ";",
		"set-hook", "-t", "="+viewer+":", "after-display-message", "", ";",
		"set-hook", "-t", "="+viewer+":", "after-capture-pane", "", ";",
		"link-window", "-k", "-s", c.WindowID, "-t", "="+viewer+":=__init")
	// new-session prints the newly owned numeric session ID before later
	// commands can fail. Never clean up by name on a duplicate-name failure.
	first, _, _ := strings.Cut(string(out), "\n")
	if controlNumericID(first, '$') {
		c.session = first
	}
	if err != nil || c.session == "" {
		if err == nil {
			err = fmt.Errorf("tmux control: invalid new session ID %q", out)
		}
		return nil, errors.Join(err, c.Close())
	}
	args := []string{"-u", "-C", "attach-session", "-t", "=" + viewer}
	if !rw {
		args = append(args, "-f", "read-only")
	}
	c.cmd = exec.CommandContext(ctx, "tmux", args...)
	c.cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8")
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	// An explicit pipe lets Wait reap promptly without closing buffered stdout
	// before ReadEvent has consumed the final replies and %exit notification.
	r, w, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.stdout, c.cmd.Stdout = r, w
	c.reader = bufio.NewReader(r)
	err = c.cmd.Start()
	_ = w.Close()
	if err != nil {
		return nil, errors.Join(err, c.Close())
	}
	c.done = make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(c.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.done:
			// Reap the session even if the caller has not yet consumed EOF.
			_ = c.killSession()
		}
	}()
	return c, nil
}

func (c *Control) killSession() error {
	if c.session == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	out, err := controlRun(ctx, "-N", "kill-session", "-t", c.session)
	message := strings.TrimSpace(string(out))
	missing := strings.Contains(message, "can't find session") || strings.Contains(message, "session not found") ||
		strings.HasPrefix(message, "no server running on ") || message == "no sessions" ||
		(strings.HasPrefix(message, "error connecting to ") && strings.HasSuffix(message, " (No such file or directory)"))
	if err != nil && missing {
		return nil
	}
	return err
}

// Close is idempotent and destroys only the viewer, never the shared window.
func (c *Control) Close() error {
	c.once.Do(func() {
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		if c.done != nil {
			<-c.done
		}
		c.closeErr = c.killSession()
	})
	return c.closeErr
}

// Send is not a command allowlist: callers must use fixed commands and hex input.
func (c *Control) Send(line string) error {
	if strings.TrimSpace(line) == "" || strings.ContainsAny(line, "\x00\r\n") {
		return errors.New("tmux control: expected one nonempty command line")
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.stdin == nil {
		return io.ErrClosedPipe
	}
	n, err := io.WriteString(c.stdin, line+"\n")
	if err == nil && n != len(line)+1 {
		err = io.ErrShortWrite
	}
	return err
}

// decodeControl decodes tmux's three-octal-digit byte escapes, not Go string
// escapes. All other bytes, including invalid UTF-8, pass through unchanged.
func decodeControl(data []byte) ([]byte, error) {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			out = append(out, data[i])
			continue
		}
		if i+3 >= len(data) || data[i+1] < '0' || data[i+1] > '3' || data[i+2] < '0' || data[i+2] > '7' || data[i+3] < '0' || data[i+3] > '7' {
			return nil, fmt.Errorf("tmux control: invalid octal escape at byte %d", i)
		}
		out = append(out, (data[i+1]-'0')*64+(data[i+2]-'0')*8+data[i+3]-'0')
		i += 3
	}
	return out, nil
}

func blockIdentity(line, kind string) (string, bool) {
	prefix := "%" + kind + " "
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	id := strings.TrimPrefix(line, prefix)
	fields := strings.Split(id, " ")
	return id, len(fields) == 3 && decimal(fields[0]) && decimal(fields[1]) && decimal(fields[2])
}

func (c *Control) readLine() (string, error) {
	// No Scanner limit: a single output line or capture can exceed 64 KiB.
	line, err := c.reader.ReadString('\n')
	if err != nil {
		if len(line) != 0 && errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	return strings.TrimSuffix(line, "\n"), nil
}

// Reply data can start with %end, %error, %output or %exit. Only an exact
// matching terminator, including flags, ends the block. tmux defers real
// notifications until after that terminator.
func (c *Control) ReadEvent() (ControlEvent, error) {
	for {
		line, err := c.readLine()
		if err != nil {
			return ControlEvent{}, err
		}
		if id, ok := blockIdentity(line, "begin"); ok {
			var data bytes.Buffer
			for {
				part, err := c.readLine()
				if err != nil {
					if errors.Is(err, io.EOF) {
						err = io.ErrUnexpectedEOF
					}
					return ControlEvent{}, err
				}
				end, ended := blockIdentity(part, "end")
				failed, errored := blockIdentity(part, "error")
				if ended && end == id || errored && failed == id {
					if !strings.HasSuffix(id, " 1") {
						break
					}
					return ControlEvent{Kind: "reply", Data: bytes.TrimSuffix(data.Bytes(), []byte{'\n'}), Failed: errored}, nil
				}
				if strings.HasSuffix(id, " 1") {
					data.WriteString(part)
					data.WriteByte('\n')
				}
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "%output "):
			pane, text, ok := strings.Cut(strings.TrimPrefix(line, "%output "), " ")
			if !ok || !controlNumericID(pane, '%') {
				return ControlEvent{}, fmt.Errorf("tmux control: malformed output %q", line)
			}
			data, err := decodeControl([]byte(text))
			return ControlEvent{Kind: "output", PaneID: pane, Data: data}, err
		case strings.HasPrefix(line, "%layout-change "):
			window, layout, ok := strings.Cut(strings.TrimPrefix(line, "%layout-change "), " ")
			if !ok || !controlNumericID(window, '@') {
				return ControlEvent{}, fmt.Errorf("tmux control: malformed layout %q", line)
			}
			return ControlEvent{Kind: "layout", WindowID: window, Data: []byte(layout)}, nil
		case line == "%exit" || strings.HasPrefix(line, "%exit "):
			return ControlEvent{Kind: "exit", Data: []byte(strings.TrimPrefix(strings.TrimPrefix(line, "%exit"), " "))}, nil
		}
		// Session/window notifications, notably %window-close, are not exits.
	}
}

// Qualify the pane with the viewer: a bare %N can resolve to the main
// session, bypassing the viewer's empty after-hooks.
func (c *Control) paneTarget() string {
	return "=" + c.viewer + ":" + c.WindowID + "." + c.PaneID
}

func (c *Control) SizeCommand() string {
	return fmt.Sprintf("display-message -p -t %s '#{pane_width} #{pane_height}'", c.paneTarget())
}

func (c *Control) ResizeCommand(cols, rows int) string {
	return fmt.Sprintf("refresh-client -C %dx%d ; select-window -t =%s:", cols, rows, c.viewer)
}

// window_panes guards against a window becoming multi-pane after setup.
// The final field contains mode flags in snapshotFlagModes order.
// tmux before 3.7 lacks paste and synchronized-output formats; use zero.
const snapshotFormat = "#{pane_width}|#{pane_height}|#{cursor_x}|#{cursor_y}|#{alternate_on}|#{history_size}|#{pane_private_modes}|#{keypad_flag}|#{insert_flag}|#{scroll_region_upper}|#{scroll_region_lower}|#{window_panes}|#{keypad_cursor_flag}#{origin_flag}#{wrap_flag}#{cursor_flag}#{mouse_standard_flag}#{mouse_button_flag}#{mouse_all_flag}#{mouse_utf8_flag}#{mouse_sgr_flag}#{?bracket_paste_flag,1,0}#{?synchronized_output_flag,1,0}"

var snapshotFlagModes = [...]int{1, 6, 7, 25, 1000, 1002, 1003, 1005, 1006, 2004, 2026}

// The five commands form a capture barrier. Callers must suppress live output
// until all replies arrive. With no history, -E -1 captures visible row zero;
// BuildSnapshot discards that duplicate.
func (c *Control) SnapshotCommand(depth int) string {
	format := snapshotFormat
	if depth <= 0 {
		format = strings.Replace(format, "#{history_size}", "0", 1)
	}
	return fmt.Sprintf("display-message -p -t %s '%s' ; capture-pane -p -e -t %s -S -%d -E -1 ; capture-pane -p -e -a -q -t %s ; capture-pane -p -e -t %s -S 0 -E - ; capture-pane -p -P -C -t %s", c.paneTarget(), format, c.paneTarget(), max(1, depth), c.paneTarget(), c.paneTarget(), c.paneTarget())
}

type Snapshot struct {
	Cols, Rows int
	Data       []byte
}

func snapshotModes(privateModes, flags string) (map[int]bool, error) {
	if len(flags) != len(snapshotFlagModes) || strings.Trim(flags, "01") != "" {
		return nil, fmt.Errorf("tmux snapshot: invalid mode flags %q", flags)
	}
	modes := make(map[int]bool)
	if privateModes == "" {
		// tmux 3.7c expands the unknown pane_private_modes to an empty string.
		// Existing flags recover exposed modes without assuming defaults.
		// A genuinely empty mode list also has all these flags unset.
		for i, n := range snapshotFlagModes {
			modes[n] = flags[i] == '1'
		}
		return modes, nil
	}
	for f := range strings.SplitSeq(privateModes, ",") {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > 9999 {
			return nil, fmt.Errorf("tmux snapshot: invalid private mode %q", f)
		}
		modes[n] = true
	}
	return modes, nil
}

// Capture SGR state continues across lines, so resets belong at capture
// boundaries. tmux cannot expose the current pen, saved cursor, character sets,
// tab stops or pending-wrap state for exact restoration.
func BuildSnapshot(replies [][]byte) (Snapshot, error) {
	if len(replies) != 5 {
		return Snapshot{}, fmt.Errorf("tmux snapshot: expected five replies, got %d", len(replies))
	}
	fields := strings.Split(string(replies[0]), "|")
	if len(fields) != 13 {
		return Snapshot{}, errors.New("tmux snapshot: invalid state format")
	}
	var state [12]int
	for i, f := range fields[:12] {
		if i == 6 {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return Snapshot{}, fmt.Errorf("tmux snapshot: invalid state field %d: %q", i, f)
		}
		state[i] = n
	}
	cols, rows, x, y := state[0], state[1], state[2], state[3]
	alt, history, keypad, insert, top, bottom := state[4], state[5], state[7], state[8], state[9], state[10]
	if cols < 1 || rows < 1 || x > cols || y >= rows || alt > 1 || keypad > 1 || insert > 1 || top > bottom || bottom >= rows || state[11] != 1 {
		return Snapshot{}, errors.New("tmux snapshot: invalid geometry, modes or pane count")
	}
	modes, err := snapshotModes(fields[6], fields[12])
	if err != nil {
		return Snapshot{}, err
	}
	pending, err := decodeControl(replies[4])
	if err != nil {
		return Snapshot{}, err
	}
	var b bytes.Buffer
	b.WriteString("\x1b[0m\x1b[?1049l\x1b[?6l\x1b[?7h\x1b[r\x1b[H\x1b[2J")
	paint := func(data []byte) {
		b.WriteString("\x1b[0m")
		b.Write(bytes.ReplaceAll(data, []byte{'\n'}, []byte{'\r', '\n'}))
		b.WriteString("\x1b[0m")
	}
	if history > 0 {
		paint(replies[1])
		b.WriteString("\r\n")
	}
	if alt == 1 {
		paint(replies[2]) // Seed the normal screen before entering the alternate.
		b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")
	}
	paint(replies[3])
	fmt.Fprintf(&b, "\x1b[%d;%dr", top+1, bottom+1)
	// Explicitly restore defaults too: RIS/xterm.reset enable 7 and 25,
	// whereas an unset pane mode means they must be disabled.
	for _, n := range []int{1, 6, 7, 12, 25, 1000, 1002, 1003, 1004, 1005, 1006, 2004, 2026, 2031} {
		setting := 'l'
		if modes[n] {
			setting = 'h'
		}
		fmt.Fprintf(&b, "\x1b[?%d%c", n, setting)
	}
	if keypad == 1 {
		b.WriteString("\x1b=")
	} else {
		b.WriteString("\x1b>")
	}
	if insert == 1 {
		b.WriteString("\x1b[4h")
	} else {
		b.WriteString("\x1b[4l")
	}
	if modes[6] {
		y = max(0, y-top) // CUP is relative to the scroll region in origin mode.
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, min(x, cols-1)+1)
	b.Write(pending) // Last: even a cursor command would cancel an unfinished CSI.
	return Snapshot{Cols: cols, Rows: rows, Data: b.Bytes()}, nil
}
