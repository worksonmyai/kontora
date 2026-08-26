package assistant

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLiveText(t *testing.T) {
	tests := []struct {
		name string
		run  func(*LiveText)
		want Partial
	}{
		{
			name: "a thread that never started has no buffer",
			run:  func(l *LiveText) { l.Append("t", "hello") },
			want: Partial{},
		},
		{
			name: "a delta with no block of its own still opens one",
			run: func(l *LiveText) {
				l.Start("t")
				l.Append("t", "hi")
			},
			want: Partial{Gen: 1, Text: "hi", Running: true, Present: true},
		},
		{
			name: "a new block replaces the text and moves the generation on",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "first")
				l.Block("t")
				l.Append("t", "second")
			},
			want: Partial{Gen: 2, Text: "second", Running: true, Present: true},
		},
		{
			name: "sealing stops the caret",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "done")
				l.Seal("t")
			},
			want: Partial{Gen: 1, Text: "done", Sealed: true, Running: true, Present: true},
		},
		{
			name: "the tool call is named while its arguments are generated",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "running it")
				l.Tool("t", "Bash")
			},
			want: Partial{Gen: 1, Text: "running it", Tool: "Bash", Running: true, Present: true},
		},
		{
			name: "the turn ending keeps the text and drops the pending tool",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "died mid-sentence")
				l.Tool("t", "Bash")
				l.End("t")
			},
			want: Partial{Gen: 1, Text: "died mid-sentence", Sealed: true, Present: true},
		},
		{
			name: "a suppressed generation reports nothing",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "landed")
				l.Seal("t")
				l.Suppress("t", 1)
			},
			want: Partial{Gen: 1, Sealed: true, Running: true, Present: true},
		},
		{
			name: "suppressing a generation the agent has moved past does nothing",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "old")
				l.Block("t")
				l.Append("t", "new")
				l.Suppress("t", 1)
			},
			want: Partial{Gen: 2, Text: "new", Running: true, Present: true},
		},
		{
			name: "a cleared thread is not brought back by a late delta",
			run: func(l *LiveText) {
				l.Start("t")
				l.Append("t", "gone")
				l.Clear("t")
				l.Append("t", "later")
			},
			want: Partial{},
		},
		{
			name: "a new turn drops what the last one typed",
			run: func(l *LiveText) {
				l.Start("t")
				l.Block("t")
				l.Append("t", "turn one")
				l.End("t")
				l.Start("t")
			},
			want: Partial{Running: true, Present: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := NewLiveText()
			tt.run(l)
			assert.Equal(t, tt.want, l.Snapshot("t"))
		})
	}
}

func TestLiveTextCap(t *testing.T) {
	l := NewLiveText()
	l.Start("t")
	l.Block("t")
	l.Append("t", strings.Repeat("a", PartialMax-2))
	l.Append("t", "bbbb")
	l.Append("t", "cccc")

	got := l.Snapshot("t")
	assert.Equal(t, PartialMax, len(got.Text))
	assert.True(t, got.Truncated)
	assert.Equal(t, strings.Repeat("a", PartialMax-2)+"bb", got.Text)
}

func TestLiveTextPrunesFinishedThreads(t *testing.T) {
	l := NewLiveText()
	now := time.Now()
	l.now = func() time.Time { return now }

	l.Start("old")
	l.Append("old", "stale")
	l.End("old")
	l.Start("running")

	now = now.Add(2 * partialTTL)
	// Only Start prunes, so the map is bounded without a goroutine of its own.
	l.Start("new")

	assert.False(t, l.Snapshot("old").Present)
	assert.True(t, l.Snapshot("running").Present, "a turn still going is not pruned however long it runs")
	assert.True(t, l.Snapshot("new").Present)
}

func TestLiveTextConcurrentAppendAndSnapshot(t *testing.T) {
	l := NewLiveText()
	l.Start("t")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 500 {
			l.Append("t", "x")
		}
	}()
	go func() {
		defer wg.Done()
		for range 500 {
			l.Snapshot("t")
		}
	}()
	wg.Wait()
	assert.Equal(t, 500, len(l.Snapshot("t").Text))
}
