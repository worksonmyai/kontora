package web

import (
	"bytes"
	"context"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/tmux"
)

func TestPaneQueue(t *testing.T) {
	for _, tt := range []struct {
		name        string
		count, size int
	}{
		{"packet limit", 257, 1},
		{"byte limit", 3, 1024 * 1024},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := newPaneQueue()
			for range tt.count - 1 {
				require.True(t, q.push(panePacket{data: bytes.Repeat([]byte("x"), tt.size)}))
			}
			require.False(t, q.push(panePacket{data: bytes.Repeat([]byte("y"), tt.size)}))
			assert.Empty(t, q.packets)
			assert.Zero(t, q.bytes)
			q.snapshot(tmux.Snapshot{Cols: 80, Rows: 24, Data: []byte("new snapshot")})
			require.True(t, q.push(panePacket{data: []byte("live")}))
			ctx := t.Context()
			p, ok := q.pop(ctx)
			require.True(t, ok)
			assert.True(t, p.reset)
			assert.Equal(t, "new snapshot", string(p.data))
			p, ok = q.pop(ctx)
			require.True(t, ok)
			assert.False(t, p.reset)
			assert.Equal(t, "live", string(p.data))
			assert.Zero(t, q.bytes)
		})
	}
}

type paneFrameRecorder struct {
	types  []websocket.MessageType
	frames [][]byte
	stop   context.CancelFunc
	want   int
}

func (r *paneFrameRecorder) Write(_ context.Context, typ websocket.MessageType, data []byte) error {
	r.types = append(r.types, typ)
	r.frames = append(r.frames, bytes.Clone(data))
	if r.stop != nil && len(r.frames) == r.want {
		r.stop()
	}
	return nil
}

func TestWritePanePackets(t *testing.T) {
	for _, tt := range []struct {
		name   string
		finish bool
	}{
		{"canceled", false},
		{"drains after exit", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := newPaneQueue()
			data := bytes.Repeat([]byte("s"), 2*ptyReadBufSize+1)
			q.snapshot(tmux.Snapshot{Cols: 200, Rows: 50, Data: data})
			require.True(t, q.push(panePacket{data: []byte("live")}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := &paneFrameRecorder{want: 5}
			if tt.finish {
				q.finish()
			} else {
				r.stop = cancel
			}
			err := writePanePackets(ctx, r, q)
			if tt.finish {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Len(t, r.frames, 5)
			assert.Equal(t, websocket.MessageText, r.types[0])
			assert.JSONEq(t, `{"type":"reset","cols":200,"rows":50}`, string(r.frames[0]))
			for i := 1; i < len(r.frames); i++ {
				assert.Equal(t, websocket.MessageBinary, r.types[i])
				assert.LessOrEqual(t, len(r.frames[i]), ptyReadBufSize)
			}
			assert.Equal(t, data, bytes.Join(r.frames[1:4], nil))
			assert.Equal(t, "live", string(r.frames[4]))
		})
	}
}
