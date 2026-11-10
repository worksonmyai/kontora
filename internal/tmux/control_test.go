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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func protocolControl(s string) *Control {
	return &Control{reader: bufio.NewReader(strings.NewReader(s))}
}

func TestControlOutput(t *testing.T) {
	for _, tt := range []struct {
		name, encoded string
		want          []byte
	}{
		{"escape", `\033[31mred\033[0m\015\012`, []byte("\x1b[31mred\x1b[0m\r\n")},
		{"backslash", `a\134033`, []byte(`a\033`)},
		{"utf8", "héllo 世界", []byte("héllo 世界")},
		{"high octal", `\200\377`, []byte{0x80, 0xff}},
		{"raw high bytes", string([]byte{0x80, 0xff}), []byte{0x80, 0xff}},
		{"spaces", " leading  and trailing ", []byte(" leading  and trailing ")},
		{"empty", "", []byte{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := protocolControl("%output %123 " + tt.encoded + "\n")
			e, err := c.ReadEvent()
			require.NoError(t, err)
			assert.Equal(t, ControlEvent{Kind: "output", PaneID: "%123", Data: tt.want}, e)
		})
	}
	for _, bad := range []string{`\`, `\03`, `\400`, `\089`, `\x1b`, `%output missing-id text`} {
		t.Run("invalid-"+bad, func(t *testing.T) {
			_, err := protocolControl("%output %1 " + bad + "\n").ReadEvent()
			if strings.HasPrefix(bad, "%output") {
				_, err = protocolControl(bad + "\n").ReadEvent()
			}
			require.Error(t, err)
		})
	}
}

func TestControlBlocks(t *testing.T) {
	for _, tt := range []struct {
		name, input string
		want        []ControlEvent
	}{
		{"literal terminators and notifications", "%begin 100 2 1\n%end text\n%end 100 3 1\n%error 100 2 0\n%end 100 2 0\n%exit\n%layout-change @0 literal\n%output %0 literal\n%end 100 2 1\n",
			[]ControlEvent{{Kind: "reply", Data: []byte("%end text\n%end 100 3 1\n%error 100 2 0\n%end 100 2 0\n%exit\n%layout-change @0 literal\n%output %0 literal")}}},
		{"failure", "%begin 100 2 1\nno such pane\n%error 100 2 1\n", []ControlEvent{{Kind: "reply", Data: []byte("no such pane"), Failed: true}}},
		{"flags zero ignored", "%begin 100 2 0\nhook\n%end 100 2 1\n%end 100 2 0\n%begin 100 3 0\nfailed hook\n%error 100 3 0\n%begin 100 4 1\nok\n%end 100 4 1\n", []ControlEvent{{Kind: "reply", Data: []byte("ok")}}},
		{"deferred notifications", "%begin 100 2 1\none\n%end 100 2 1\n%begin 100 3 1\ntwo\n%end 100 3 1\n%layout-change @42 checksum,80x24 other flags\n%output %7 after\n%window-close @42\n%exit session gone\n",
			[]ControlEvent{{Kind: "reply", Data: []byte("one")}, {Kind: "reply", Data: []byte("two")}, {Kind: "layout", WindowID: "@42", Data: []byte("checksum,80x24 other flags")}, {Kind: "output", PaneID: "%7", Data: []byte("after")}, {Kind: "exit", Data: []byte("session gone")}}},
		{"empty reply and blank lines", "%begin 100 1 1\n%end 100 1 1\n%begin 100 2 1\n\n\n%end 100 2 1\n%exit\n", []ControlEvent{{Kind: "reply", Data: nil}, {Kind: "reply", Data: []byte("\n")}, {Kind: "exit", Data: []byte{}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := protocolControl(tt.input)
			for _, want := range tt.want {
				e, err := c.ReadEvent()
				require.NoError(t, err)
				assert.Equal(t, want, e)
			}
			_, err := c.ReadEvent()
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

func TestControlIncompleteAndLargeLines(t *testing.T) {
	for _, input := range []string{"%output %1 partial", "%begin 1 2 1\n", "%begin 1 2 1\ntext\n", "%begin 1 2 0\ntext\n"} {
		_, err := protocolControl(input).ReadEvent()
		require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	}
	large := strings.Repeat("x", 1024*1024)
	c := protocolControl("%begin 1 2 1\n" + large + "\n%end 1 2 1\n%output %1 " + large + "\n")
	for _, kind := range []string{"reply", "output"} {
		e, err := c.ReadEvent()
		require.NoError(t, err)
		assert.Equal(t, kind, e.Kind)
		assert.Equal(t, large, string(e.Data))
	}
	for _, input := range []string{"%layout-change invalid layout\n", "%output invalid bytes\n"} {
		_, err := protocolControl(input).ReadEvent()
		require.Error(t, err)
	}
}

type controlBuffer struct{ bytes.Buffer }

func (*controlBuffer) Close() error { return nil }

func TestControlSend(t *testing.T) {
	var b controlBuffer
	c := &Control{stdin: &b}
	for _, bad := range []string{"", " ", "\t", "cmd\n", "\ncmd", "cmd\rnext", "cmd\x00"} {
		require.Error(t, c.Send(bad))
	}
	assert.Empty(t, b.String())
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { assert.NoError(t, c.Send("display-message -p -t %1 'ok'")) })
	}
	wg.Wait()
	assert.Equal(t, strings.Repeat("display-message -p -t %1 'ok'\n", 20), b.String())
	require.ErrorIs(t, (&Control{}).Send("command"), io.ErrClosedPipe)
}

func TestControlCommands(t *testing.T) {
	c := &Control{PaneID: "%12", WindowID: "@3", viewer: "kontora-view-generated-123"}
	assert.Equal(t, "display-message -p -t =kontora-view-generated-123:@3.%12 '#{pane_width} #{pane_height}'", c.SizeCommand())
	assert.Equal(t, "refresh-client -C 210x55 ; select-window -t =kontora-view-generated-123:", c.ResizeCommand(210, 55))
	line := c.SnapshotCommand(5000)
	commands := strings.Split(line, " ; ")
	require.Len(t, commands, 5)
	assert.Equal(t, "display-message -p -t =kontora-view-generated-123:@3.%12 '"+snapshotFormat+"'", commands[0])
	assert.Equal(t, "capture-pane -p -e -t =kontora-view-generated-123:@3.%12 -S -5000 -E -1", commands[1])
	assert.Equal(t, "capture-pane -p -e -a -q -t =kontora-view-generated-123:@3.%12", commands[2])
	assert.Equal(t, "capture-pane -p -e -t =kontora-view-generated-123:@3.%12 -S 0 -E -", commands[3])
	assert.Equal(t, "capture-pane -p -P -C -t =kontora-view-generated-123:@3.%12", commands[4])
	assert.NotContains(t, c.SnapshotCommand(0), "#{history_size}")
	assert.NotContains(t, c.SnapshotCommand(-1), "--1")
	for _, tt := range []struct {
		id     string
		prefix byte
		valid  bool
	}{{"%0", '%', true}, {"@123", '@', true}, {"$1", '$', true}, {"%", '%', false}, {"%1;kill-server", '%', false}, {"%1\n", '%', false}, {"@1", '%', false}, {"%١", '%', false}} {
		assert.Equal(t, tt.valid, controlNumericID(tt.id, tt.prefix), "%q", tt.id)
	}
}

func snapshotReplies(state, flags, history, saved, visible, pending string) [][]byte {
	return [][]byte{[]byte(state + "|" + flags), []byte(history), []byte(saved), []byte(visible), []byte(pending)}
}

func TestBuildSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name, state, history, saved, visible, pending string
		contains, absent                              []string
		last                                          string
	}{
		{name: "normal", state: "80|24|3|4|0|2|7,25|0|0|0|23|1", history: "old1\nold2", visible: "\x1b[31mred\nstill-red", contains: []string{"old1\r\nold2\x1b[0m\r\n\x1b[0m\x1b[31mred\r\nstill-red", "\x1b[?7h", "\x1b[?25h"}, absent: []string{"\x1b[?1049h"}, last: "\x1b[5;4H"},
		{name: "alternate", state: "80|24|0|0|1|1|7,25|0|0|0|23|1", history: "history", saved: "saved-normal", visible: "alternate", contains: []string{"history", "saved-normal\x1b[0m\x1b[?1049h\x1b[H\x1b[2J\x1b[0malternate"}, last: "\x1b[1;1H"},
		{name: "no history", state: "80|24|0|0|0|0|7,25|0|0|0|23|1", history: "duplicate-visible-row-zero", visible: "visible", absent: []string{"duplicate-visible-row-zero"}, last: "\x1b[1;1H"},
		{name: "modes absent", state: "80|24|0|0|0|0||0|0|0|23|1", contains: []string{"\x1b[?7l", "\x1b[?25l", "\x1b[?2004l"}, last: "\x1b[1;1H"},
		{name: "paste keypad insert", state: "80|24|0|0|0|0|7,25,2004|1|1|0|23|1", contains: []string{"\x1b[?2004h", "\x1b=\x1b[4h"}, last: "\x1b[1;1H"},
		{name: "scroll region origin", state: "80|24|5|6|0|0|6,7,25|0|0|2|20|1", contains: []string{"\x1b[3;21r", "\x1b[?6h"}, last: "\x1b[5;6H"},
		{name: "pending last", state: "80|24|2|3|0|0|7,25|0|0|0|23|1", pending: `\033[38;5;\134`, last: "\x1b[4;3H\x1b[38;5;\\"},
		{name: "wrap pending cursor", state: "80|24|80|23|0|0|7,25|0|0|0|23|1", last: "\x1b[24;80H"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, err := BuildSnapshot(snapshotReplies(tt.state, "00000000000", tt.history, tt.saved, tt.visible, tt.pending))
			require.NoError(t, err)
			assert.Equal(t, 80, s.Cols)
			assert.Equal(t, 24, s.Rows)
			for _, part := range tt.contains {
				assert.Contains(t, string(s.Data), part)
			}
			for _, part := range tt.absent {
				assert.NotContains(t, string(s.Data), part)
			}
			assert.True(t, bytes.HasSuffix(s.Data, []byte(tt.last)), "%q", s.Data)
		})
	}
}

func TestBuildSnapshotModeFormats(t *testing.T) {
	for _, tt := range []struct {
		name, modes, flags string
		enabled            []int
	}{
		{"supported list", "1,6,7,12,25,1000,1002,1003,1004,1005,1006,2004,2026,2031", "00000000000", []int{1, 6, 7, 12, 25, 1000, 1002, 1003, 1004, 1005, 1006, 2004, 2026, 2031}},
		{"supported list is authoritative", "12,1004,2031", "11111111111", []int{12, 1004, 2031}},
		{"supported empty list", "", "00000000000", nil},
		{"unsupported format with wrap cursor paste", "", "00110000010", []int{7, 25, 2004}},
		{"unsupported format with modes disabled", "", "00000000000", nil},
		{"unsupported format with other flags", "", "11001111101", []int{1, 6, 1000, 1002, 1003, 1005, 1006, 2026}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := "80|24|0|0|0|0|" + tt.modes + "|0|0|0|23|1"
			s, err := BuildSnapshot(snapshotReplies(state, tt.flags, "", "", "", ""))
			require.NoError(t, err)
			restore := string(s.Data[bytes.LastIndex(s.Data, []byte("\x1b[1;24r")):])
			enabled := make(map[int]bool)
			for _, mode := range tt.enabled {
				enabled[mode] = true
			}
			for _, mode := range []int{1, 6, 7, 12, 25, 1000, 1002, 1003, 1004, 1005, 1006, 2004, 2026, 2031} {
				on, off := 'l', 'h'
				if enabled[mode] {
					on, off = off, on
				}
				assert.Contains(t, restore, fmt.Sprintf("\x1b[?%d%c", mode, on))
				assert.NotContains(t, restore, fmt.Sprintf("\x1b[?%d%c", mode, off))
			}
		})
	}
}

func TestBuildSnapshotRejectsInvalid(t *testing.T) {
	_, err := BuildSnapshot(nil)
	require.Error(t, err)
	for _, state := range []string{"", "80 24", "0|24|0|0|0|0||0|0|0|23|1", "80|24|81|0|0|0||0|0|0|23|1", "80|24|0|24|0|0||0|0|0|23|1", "80|24|0|0|2|0||0|0|0|23|1", "80|24|0|0|0|-1||0|0|0|23|1", "80|24|0|0|0|0||0|0|20|19|1", "80|24|0|0|0|0||0|0|0|24|1", "80|24|0|0|0|0||0|0|0|23|2", "80|24|0|0|0|0|25;kill-server|0|0|0|23|1", "80|24|0|0|0|0||x|0|0|23|1"} {
		_, err := BuildSnapshot(snapshotReplies(state, "00000000000", "", "", "", ""))
		require.Error(t, err, state)
	}
	for _, flags := range []string{"", "0011000001", "001100000100", "0011000001x", "00110000012"} {
		_, err := BuildSnapshot(snapshotReplies("80|24|0|0|0|0||0|0|0|23|1", flags, "", "", "", ""))
		require.Error(t, err, flags)
	}
	_, err = BuildSnapshot(snapshotReplies("80|24|0|0|0|0||0|0|0|23|1", "00000000000", "", "", "", `\bad`))
	require.Error(t, err)
}

func controlTmux(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).CombinedOutput()
	require.NoError(t, err, "%s", out)
	return strings.TrimSuffix(string(out), "\n")
}

type controlResult struct {
	event ControlEvent
	err   error
}

func controlEvents(c *Control) <-chan controlResult {
	ch := make(chan controlResult, 256)
	go func() {
		defer close(ch)
		for {
			e, err := c.ReadEvent()
			ch <- controlResult{e, err}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

func controlReplies(t *testing.T, ch <-chan controlResult, count int) [][]byte {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var replies [][]byte
	for len(replies) < count {
		select {
		case r := <-ch:
			require.NoError(t, r.err)
			require.NotEqual(t, "exit", r.event.Kind)
			if r.event.Kind == "reply" {
				require.False(t, r.event.Failed, "%s", r.event.Data)
				replies = append(replies, r.event.Data)
			}
		case <-timer.C:
			t.Fatal("timed out waiting for control replies")
		}
	}
	return replies
}

func TestControlLifecycleAndSnapshots(t *testing.T) {
	skipIfNoTmux(t)
	name := "control ; display-message -p INJECTED-" + randomSuffix()
	startTestWindow(t, name, t.TempDir(), "sh", "-c", "printf 'snapshot-marker\\n'; exec sleep 600")
	t.Cleanup(func() { _ = KillWindow(testSession, name) })
	viewer := "kontora-view-" + randomSuffix()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := StartControl(ctx, testSession, name, viewer, false)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, c.Close()) })
	assert.True(t, controlNumericID(c.PaneID, '%'))
	assert.True(t, controlNumericID(c.WindowID, '@'))
	assert.Equal(t, c.WindowID, controlTmux(t, "list-windows", "-t", "="+viewer, "-F", "#{window_id}"))
	ch := controlEvents(c)
	require.NoError(t, c.Send(c.ResizeCommand(100, 30)))
	controlReplies(t, ch, 2)
	require.NoError(t, c.Send(c.SizeCommand()))
	assert.Equal(t, "100 30", string(controlReplies(t, ch, 1)[0]))
	assert.Equal(t, "1 1", controlTmux(t, "list-clients", "-t", "="+viewer, "-F", "#{client_control_mode} #{client_readonly}"))

	// Global and main-session after-hooks must not interrupt the capture barrier.
	// Qualifying targets matters: a bare %N can resolve in the main session.
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "run-shell 'touch " + marker + "'"
	for _, h := range []string{"after-display-message", "after-capture-pane"} {
		controlTmux(t, "set-hook", "-g", h, hook)
		controlTmux(t, "set-hook", "-t", "="+testSession+":", h, hook)
		t.Cleanup(func() {
			controlTmux(t, "set-hook", "-gu", h)
			controlTmux(t, "set-hook", "-u", "-t", "="+testSession+":", h)
		})
	}
	require.NoError(t, c.Send(c.SnapshotCommand(5000)))
	replies := controlReplies(t, ch, 5)
	s, err := BuildSnapshot(replies)
	require.NoError(t, err)
	assert.Equal(t, 100, s.Cols)
	assert.Equal(t, 30, s.Rows)
	assert.Equal(t, 1, bytes.Count(s.Data, []byte("snapshot-marker")), "history_size=0 must not duplicate visible row zero")
	_, err = os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "capture hooks must be masked in the viewer")

	require.NoError(t, c.Send("send-keys -H -t "+c.PaneID+" 61"))
	for {
		select {
		case r := <-ch:
			require.NoError(t, r.err)
			if r.event.Kind != "reply" {
				continue
			}
			assert.True(t, r.event.Failed)
			assert.Contains(t, string(r.event.Data), "read-only")
		case <-ctx.Done():
			t.Fatal("no read-only failure reply")
		}
		break
	}
	require.NoError(t, c.Close())
	require.NoError(t, c.Close())
	assert.Error(t, c.Send(c.SizeCommand()))
	assert.True(t, HasWindow(testSession, name), "closing viewer must not kill the agent")
	require.Eventually(t, func() bool {
		return exec.Command("tmux", "has-session", "-t", "="+viewer).Run() != nil
	}, 3*time.Second, 10*time.Millisecond)
}

func TestControlSnapshotModeFormats(t *testing.T) {
	skipIfNoTmux(t)
	for _, tt := range []struct {
		name, sequences, flags string
		set                    bool
	}{
		{"enabled", `\033[?7;25;2004h`, "00110000010", true},
		{"disabled", "", "00000000000", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name := "control-modes-" + randomSuffix()
			startTestWindow(t, name, t.TempDir(), "sh", "-c", "printf '\\033[?1;6;7;12;25;1000;1002;1003;1004;1005;1006;2004;2026;2031l"+tt.sequences+"mode-ready'; exec sleep 600")
			t.Cleanup(func() { _ = KillWindow(testSession, name) })
			require.Eventually(t, func() bool {
				return strings.Contains(controlTmux(t, "capture-pane", "-p", "-t", WindowTarget(testSession, name)), "mode-ready")
			}, 3*time.Second, 10*time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c, err := StartControl(ctx, testSession, name, "kontora-view-"+randomSuffix(), false)
			require.NoError(t, err)
			defer c.Close()
			ch := controlEvents(c)
			hasPrivateModes := strings.Contains("\n"+controlTmux(t, "display-message", "-a", "-t", c.paneTarget()), "\npane_private_modes=")
			var first Snapshot
			for _, format := range []string{"installed", "unsupported"} {
				t.Run(format, func(t *testing.T) {
					line := c.SnapshotCommand(0)
					if format == "unsupported" {
						// Unknown keys expand empty, as pane_private_modes does in tmux 3.7c.
						line = strings.Replace(line, "#{pane_private_modes}", "#{kontora_unsupported_private_modes}", 1)
					}
					require.NoError(t, c.Send(line))
					replies := controlReplies(t, ch, 5)
					fields := strings.Split(string(replies[0]), "|")
					require.Len(t, fields, 13)
					assert.Equal(t, tt.flags, fields[12])
					if format == "installed" && hasPrivateModes && tt.set {
						assert.Equal(t, "7,25,2004", fields[6])
					} else {
						assert.Empty(t, fields[6])
					}
					s, err := BuildSnapshot(replies)
					require.NoError(t, err)
					restore := string(s.Data[bytes.LastIndex(s.Data, fmt.Appendf(nil, "\x1b[1;%dr", s.Rows)):])
					for _, mode := range []int{7, 25, 2004} {
						setting, opposite := 'l', 'h'
						if tt.set {
							setting, opposite = opposite, setting
						}
						assert.Contains(t, restore, fmt.Sprintf("\x1b[?%d%c", mode, setting))
						assert.NotContains(t, restore, fmt.Sprintf("\x1b[?%d%c", mode, opposite))
					}
					if format == "installed" {
						first = s
					} else {
						assert.Equal(t, first, s)
					}
				})
			}
		})
	}
}

func TestControlSetupFailures(t *testing.T) {
	skipIfNoTmux(t)
	name := "control-setup-" + randomSuffix()
	startTestWindow(t, name, t.TempDir(), "sleep", "600")
	t.Cleanup(func() { _ = KillWindow(testSession, name) })
	for _, viewer := range []string{"", "bad viewer", "bad;kill-server", "bad\n", "=bad", "bad:session"} {
		_, err := StartControl(context.Background(), testSession, name, viewer, true)
		require.Error(t, err)
	}
	viewer := "kontora-view-" + randomSuffix()
	c, err := StartControl(context.Background(), testSession, "not-a-window", viewer, true)
	require.Error(t, err)
	assert.Nil(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = StartControl(ctx, testSession, name, viewer, true)
	require.Error(t, err)

	controlTmux(t, "new-session", "-d", "-s", viewer, "--", "sleep", "600")
	t.Cleanup(func() { controlTmux(t, "kill-session", "-t", "="+viewer) })
	_, err = StartControl(context.Background(), testSession, name, viewer, true)
	require.Error(t, err)
	controlTmux(t, "has-session", "-t", "="+viewer) // A collision is not ours to delete.

	controlTmux(t, "split-window", "-d", "-t", WindowTarget(testSession, name), "--", "sleep", "600")
	_, err = StartControl(context.Background(), testSession, name, "kontora-view-"+randomSuffix(), true)
	require.ErrorContains(t, err, "expected one pane")
}

func TestControlExitAndCancellation(t *testing.T) {
	skipIfNoTmux(t)
	for _, mode := range []string{"window killed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			name := "control-exit-" + randomSuffix()
			startTestWindow(t, name, t.TempDir(), "sleep", "600")
			t.Cleanup(func() { _ = KillWindow(testSession, name) })
			viewer := "kontora-view-" + randomSuffix()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, err := StartControl(ctx, testSession, name, viewer, true)
			require.NoError(t, err)
			defer c.Close()
			ch := controlEvents(c)
			require.NoError(t, c.Send(c.SizeCommand()))
			controlReplies(t, ch, 1)
			if mode == "cancelled" {
				cancel()
			} else {
				require.NoError(t, KillWindow(testSession, name))
			}
			exited := false
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
		read:
			for {
				select {
				case r, ok := <-ch:
					if !ok || r.err != nil {
						break read
					}
					exited = exited || r.event.Kind == "exit"
				case <-timer.C:
					t.Fatal("control reader did not unblock")
				}
			}
			if mode == "window killed" {
				assert.True(t, exited, "drain buffered stdout before reaping")
			}
			require.Eventually(t, func() bool {
				return exec.Command("tmux", "has-session", "-t", "="+viewer).Run() != nil
			}, 3*time.Second, 10*time.Millisecond)
			select {
			case <-c.done:
			case <-time.After(3 * time.Second):
				t.Fatal("control child not reaped")
			}
		})
	}
}

func TestControlRWInput(t *testing.T) {
	skipIfNoTmux(t)
	name := "control-input-" + randomSuffix()
	file := filepath.Join(t.TempDir(), "typed")
	startTestWindow(t, name, t.TempDir(), "sh", "-c", "cat > "+file)
	t.Cleanup(func() { _ = KillWindow(testSession, name) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := StartControl(ctx, testSession, name, "kontora-view-"+randomSuffix(), true)
	require.NoError(t, err)
	defer c.Close()
	ch := controlEvents(c)
	text := "héllo;\"'\n"
	var hex strings.Builder
	for _, b := range []byte(text) {
		fmt.Fprintf(&hex, " %02x", b)
	}
	require.NoError(t, c.Send("send-keys -H -t "+c.PaneID+hex.String()))
	controlReplies(t, ch, 1)
	require.Eventually(t, func() bool {
		got, err := os.ReadFile(file)
		return err == nil && string(got) == text
	}, 3*time.Second, 10*time.Millisecond)
}

func TestControlPartialSetupCleanup(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "tmux"), []byte(`#!/bin/sh
case "$2" in
list-panes) printf '%s\n' "$CONTROL_TEST_IDS" ;;
new-session) printf '$52\n'; printf 'link failed\n' >&2; exit 1 ;;
-N) [ "$3" = kill-session ] && [ "$4" = -t ] && [ "$5" = '$52' ] || exit 99
    printf 'cleaned\n' > "$CONTROL_TEST_LOG" ;;
*) exit 98 ;;
esac
`), 0o755))
	t.Setenv("PATH", bin)
	t.Setenv("CONTROL_TEST_LOG", log)
	t.Setenv("CONTROL_TEST_IDS", "%1 @2")
	c, err := StartControl(context.Background(), "main", "window", "generated", true)
	require.ErrorContains(t, err, "link failed")
	assert.Nil(t, c)
	got, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, "cleaned\n", string(got), "clean up an owned partial session by numeric ID")
	require.NoError(t, os.Remove(log))
	for _, ids := range []string{"%1 @2\n%3 @2", "%1;kill-server @2", "%1 @bad", "", "junk"} {
		t.Setenv("CONTROL_TEST_IDS", ids)
		_, err := StartControl(context.Background(), "main", "window", "generated", true)
		require.ErrorContains(t, err, "expected one pane")
		_, err = os.Stat(log)
		require.ErrorIs(t, err, os.ErrNotExist, "never create or clean up a viewer for invalid IDs")
	}
}

func TestControlSetupContextTimeout(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := StartControl(ctx, "main", "window", "generated", true)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestControlCloseUnblocksSend(t *testing.T) {
	r, w := io.Pipe()
	c := &Control{stdin: w}
	defer r.Close()
	done := make(chan error, 1)
	go func() { done <- c.Send("blocked") }()
	require.NoError(t, c.Close())
	select {
	case err := <-done:
		require.True(t, errors.Is(err, io.ErrClosedPipe))
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Send")
	}
}
