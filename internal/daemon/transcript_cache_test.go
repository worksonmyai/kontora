package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestTranscriptCacheVersions(t *testing.T) {
	for _, change := range []string{"append", "replace same metadata", "truncate", "finish partial line", "parser", "mtime", "absolute alias"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			content := claudeTextJSONL("one")
			if change == "finish partial line" {
				content = strings.TrimSuffix(content, "}\n")
			}
			require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			var calls atomic.Int32
			c := transcriptCache{parse: func(r io.Reader, pi bool) (logfmt.Tape, error) { calls.Add(1); return parseTranscript(r, pi) }}
			first, v1, err := c.read(path, false)
			require.NoError(t, err)
			require.NotEmpty(t, v1)
			again, v2, err := c.read(path, false)
			require.NoError(t, err)
			assert.Equal(t, first, again)
			assert.Equal(t, v1, v2)
			assert.EqualValues(t, 1, calls.Load())
			pi := false
			nextPath := path
			switch change {
			case "append":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = f.WriteString(claudeTextJSONL("two"))
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "replace same metadata":
				st, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path+".new", []byte(claudeTextJSONL("two")), 0o644))
				require.NoError(t, os.Chtimes(path+".new", st.ModTime(), st.ModTime()))
				require.NoError(t, os.Rename(path+".new", path))
			case "truncate":
				require.NoError(t, os.Truncate(path, 0))
			case "finish partial line":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = f.WriteString("}\n")
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "parser":
				pi = true
			case "mtime":
				st, err := os.Stat(path)
				require.NoError(t, err)
				require.NoError(t, os.Chtimes(path, st.ModTime(), st.ModTime().Add(time.Second)))
			case "absolute alias":
				nextPath = filepath.Dir(path) + "/./" + filepath.Base(path)
			}
			got, v3, err := c.read(nextPath, pi)
			require.NoError(t, err)
			if change == "absolute alias" {
				assert.Equal(t, v1, v3)
				assert.EqualValues(t, 1, calls.Load())
				return
			}
			assert.NotEqual(t, v1, v3)
			assert.EqualValues(t, 2, calls.Load())
			if change == "replace same metadata" || change == "append" {
				assert.Equal(t, "two", got.Events[len(got.Events)-1].Text)
			}
			if change == "finish partial line" {
				require.NotEmpty(t, got.Events)
				assert.Equal(t, "one", got.Events[len(got.Events)-1].Text)
			}
			if change == "truncate" {
				assert.Empty(t, got.Events)
			}
		})
	}
}

func TestTranscriptCacheDetachedResponses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o644))
	c := transcriptCache{}
	full, _, err := c.read(path, true)
	require.NoError(t, err)
	expected := cloneTape(full, true)
	full.SliceAt(2)
	full.Events[0].Text = "changed"
	*full.Events[0].Time = time.Time{}
	require.NotNil(t, full.Events[0].Usage)
	full.Events[0].Usage.Input = 9999
	full.Partial = append(full.Partial, "mutation")
	got, _, err := c.read(path, true)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
	// Pi records without usage/time produce a nonempty Partial slice.
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`), 0o644))
	partial, _, err := c.read(path, true)
	require.NoError(t, err)
	require.NotEmpty(t, partial.Partial)
	expected = cloneTape(partial, true)
	partial.Partial[0] = "changed"
	got, _, err = c.read(path, true)
	require.NoError(t, err)
	assert.Equal(t, expected, got)
}

func TestTranscriptCacheConcurrentMiss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o644))
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := transcriptCache{parse: func(r io.Reader, pi bool) (logfmt.Tape, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return parseTranscript(r, pi)
	}}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			got, version, err := c.read(path, true)
			assert.NoError(t, err)
			assert.NotEmpty(t, version)
			assert.NotEmpty(t, got.Events)
		})
	}
	<-entered
	close(release)
	wg.Wait()
	assert.EqualValues(t, 1, calls.Load())
}

func TestTranscriptCacheUnstableSnapshotsAndFailedReads(t *testing.T) {
	for _, change := range []string{"append", "replace", "remove", "parse error"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o644))
			calls := 0
			c := transcriptCache{parse: func(r io.Reader, pi bool) (logfmt.Tape, error) {
				calls++
				tape, err := parseTranscript(r, pi)
				if calls > 1 {
					return tape, err
				}
				switch change {
				case "append":
					f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					require.NoError(t, e)
					_, e = f.WriteString("{}\n")
					require.NoError(t, e)
					require.NoError(t, f.Close())
				case "replace":
					require.NoError(t, os.WriteFile(path+".new", []byte(piSessionJSONL), 0o644))
					require.NoError(t, os.Rename(path+".new", path))
				case "remove":
					require.NoError(t, os.Remove(path))
				case "parse error":
					return logfmt.Tape{}, errors.New("read failed")
				}
				return tape, err
			}}
			first, version, err := c.read(path, true)
			if change == "parse error" {
				require.Error(t, err)
				assert.Empty(t, first.Events)
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, first.Events)
			}
			assert.Empty(t, version)
			assert.Empty(t, c.entries)
			assert.Empty(t, c.flights)
			if change == "remove" {
				require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o644))
			}
			got, version, err := c.read(path, true)
			require.NoError(t, err)
			assert.NotEmpty(t, version)
			assert.NotEmpty(t, got.Events)
			assert.Equal(t, 2, calls)
			require.NoError(t, os.Remove(path))
			_, version, err = c.read(path, true)
			require.ErrorIs(t, err, os.ErrNotExist)
			assert.Empty(t, version)
		})
	}
}

func TestTranscriptCacheConditionalChange(t *testing.T) {
	for _, change := range []string{"unchanged", "append", "replace", "truncate", "remove"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o600))
			calls := 0
			c := transcriptCache{parse: func(r io.Reader, pi bool) (logfmt.Tape, error) {
				calls++
				return parseTranscript(r, pi)
			}}
			first, version, err := c.read(path, true)
			require.NoError(t, err)
			got, accepted, err := c.readConditional(path, true, func(candidate string) bool {
				require.Equal(t, version, candidate)
				switch change {
				case "append":
					f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
					require.NoError(t, err)
					_, err = f.WriteString("{}\n")
					require.NoError(t, err)
					require.NoError(t, f.Close())
				case "replace":
					require.NoError(t, os.WriteFile(path+".new", []byte(piSessionJSONL), 0o600))
					require.NoError(t, os.Rename(path+".new", path))
				case "truncate":
					require.NoError(t, os.Truncate(path, 0))
				case "remove":
					require.NoError(t, os.Remove(path))
				}
				return true
			})
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
			if change == "unchanged" {
				assert.Equal(t, version, accepted)
				assert.Empty(t, got.Events)
				return
			}
			assert.Empty(t, accepted, "a matching candidate must still pass file stability checks")
			assert.Equal(t, first, got)
			assert.Len(t, c.entries, 1)
			if change == "remove" {
				_, accepted, err = c.read(path, true)
				require.ErrorIs(t, err, os.ErrNotExist)
				assert.Empty(t, accepted)
				require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o600))
			}
			_, accepted, err = c.read(path, true)
			require.NoError(t, err)
			assert.NotEmpty(t, accepted)
			assert.NotEqual(t, version, accepted)
			assert.Equal(t, 2, calls)
		})
	}
}

func TestTranscriptCacheReadAccessRecovery(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(piSessionJSONL), 0o600))
	c := transcriptCache{}
	first, version, err := c.read(path, true)
	require.NoError(t, err)
	require.NotEmpty(t, first.Events)
	require.NoError(t, os.Chmod(path, 0))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	_, failedVersion, err := c.read(path, true)
	require.Error(t, err)
	assert.Empty(t, failedVersion)
	require.NoError(t, os.Chmod(path, 0o600))
	recovered, recoveredVersion, err := c.read(path, true)
	require.NoError(t, err)
	assert.Equal(t, first, recovered)
	assert.Equal(t, version, recoveredVersion)
}

func TestTranscriptCacheLRU(t *testing.T) {
	root := t.TempDir()
	calls := 0
	c := transcriptCache{parse: func(r io.Reader, pi bool) (logfmt.Tape, error) { calls++; return parseTranscript(r, pi) }}
	for i := range transcriptCacheEntries + 1 {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte(piSessionJSONL), 0o600))
	}
	for i := range transcriptCacheEntries {
		_, _, err := c.read(filepath.Join(root, fmt.Sprint(i)), true)
		require.NoError(t, err)
	}
	_, _, err := c.read(filepath.Join(root, "0"), true)
	require.NoError(t, err)
	_, _, err = c.read(filepath.Join(root, fmt.Sprint(transcriptCacheEntries)), true)
	require.NoError(t, err)
	_, _, err = c.read(filepath.Join(root, "0"), true)
	require.NoError(t, err)
	assert.Equal(t, transcriptCacheEntries+1, calls, "the recently read entry stays cached")
	_, _, err = c.read(filepath.Join(root, "1"), true)
	require.NoError(t, err)
	assert.Equal(t, transcriptCacheEntries+2, calls, "the least recently read entry was evicted")
}

func TestTranscriptCacheLimits(t *testing.T) {
	for _, tc := range []struct {
		name             string
		entries, content int
		wantEntries      int
	}{
		{name: "entry eviction", entries: 40, content: 8, wantEntries: 32},
		{name: "content eviction", entries: 5, content: 9 << 20, wantEntries: 3},
		{name: "oversized bypass", entries: 1, content: transcriptCacheBytes + 1, wantEntries: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			calls := 0
			c := transcriptCache{parse: func(io.Reader, bool) (logfmt.Tape, error) {
				calls++
				return logfmt.Tape{Events: []logfmt.Event{{Kind: "text", Text: strings.Repeat("x", tc.content)}}}, nil
			}}
			for i := range tc.entries {
				path := filepath.Join(root, fmt.Sprint(i))
				require.NoError(t, os.WriteFile(path, []byte("{}"), 0o644))
				got, _, err := c.read(path, false)
				require.NoError(t, err)
				assert.Len(t, got.Events[0].Text, tc.content)
				assert.LessOrEqual(t, c.bytes, transcriptCacheBytes)
				assert.LessOrEqual(t, len(c.entries), transcriptCacheEntries)
			}
			assert.Len(t, c.entries, tc.wantEntries)
			var sum int
			for _, e := range c.entries {
				sum += e.bytes
			}
			assert.Equal(t, sum, c.bytes)
			_, _, err := c.read(filepath.Join(root, "0"), false)
			require.NoError(t, err)
			assert.Equal(t, tc.entries+1, calls, "evicted and oversized data must be parsed again")
		})
	}
}

func BenchmarkTranscriptCache(b *testing.B) {
	path := filepath.Join(b.TempDir(), "s.jsonl")
	content := strings.Repeat(claudeTextJSONL(strings.Repeat("x", 1024)), 2000)
	require.NoError(b, os.WriteFile(path, []byte(content), 0o644))
	for _, mode := range []string{"uncached parse", "unchanged file", "partial only"} {
		b.Run(mode, func(b *testing.B) {
			c := transcriptCache{}
			_, _, err := c.read(path, false)
			require.NoError(b, err)
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			n := 0
			for b.Loop() {
				if mode == "uncached parse" {
					_, err = sessionTape(path, false)
				} else {
					var version string
					_, version, err = c.read(path, false)
					if mode == "partial only" {
						n++
						_ = assistantETag(version, 1, web.AssistantActivityInfo{Partial: strings.Repeat("x", n%1024), PartialGen: 1})
					}
				}
				require.NoError(b, err)
			}
		})
	}
}
