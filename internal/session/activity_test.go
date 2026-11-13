package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenActivity(t *testing.T) {
	for _, tc := range []struct {
		name, stage, sidecar, log string
		run                       int
		source                    string
		wantMissing, wantBad      bool
	}{
		{name: "sidecar wins", stage: "review", sidecar: `{"version":1,"events":[]}`, log: "raw", source: "events"},
		{name: "malformed sidecar does not fall back", stage: "review", sidecar: `{bad`, log: "raw", source: "events", wantBad: true},
		{name: "trailing sidecar data is invalid", stage: "review", sidecar: `{"events":[]} {}`, source: "events", wantBad: true},
		{name: "plaintext fallback", stage: "review", log: "raw", source: "log"},
		{name: "shared plaintext across runs", stage: "review", run: 1, log: "raw", source: "log"},
		{name: "missing named stage", stage: "missing", wantMissing: true},
		{name: "different run sidecar is not borrowed", stage: "review", run: 1, wantMissing: true},
		{name: "empty stage is not newest", wantMissing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "ticket")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "other.log"), []byte("unrelated"), 0o644))
			require.NoError(t, os.WriteFile(EventsPath(root, "ticket", "review", 99), []byte(`{"events":[]}`), 0o644))
			if tc.sidecar != "" {
				require.NoError(t, os.WriteFile(EventsPath(root, "ticket", "review", 0), []byte(tc.sidecar), 0o644))
			}
			if tc.log != "" {
				require.NoError(t, os.WriteFile(LogPath(root, "ticket", "review"), []byte(tc.log), 0o644))
			}
			a, err := OpenActivity(root, "ticket", tc.stage, tc.run)
			if tc.wantMissing {
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			defer a.File.Close()
			assert.True(t, filepath.IsAbs(a.Path))
			assert.Equal(t, tc.source, a.Source)
			if a.Source == "events" {
				tape, err := a.Tape()
				if tc.wantBad {
					require.ErrorContains(t, err, "parsing activity sidecar")
					return
				}
				require.NoError(t, err)
				require.NotNil(t, tape)
			} else {
				got, start, size, err := a.Content(0)
				require.NoError(t, err)
				assert.Equal(t, tc.log, got)
				assert.Zero(t, start)
				assert.EqualValues(t, len(tc.log), size)
			}
		})
	}
}

func TestActivityLogBoundary(t *testing.T) {
	for _, mode := range []string{"sidecar escape", "log escape", "newest escape", "explicit escape", "visit log", "sidecar wins visit"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			dir := TicketDir(root, "ticket")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			outside := filepath.Join(t.TempDir(), "outside.log")
			require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o644))
			path := GraphLogPath(root, "ticket", "review", "visit")
			var a *Artifact
			var err error
			switch mode {
			case "sidecar escape":
				require.NoError(t, os.Symlink(outside, EventsPath(root, "ticket", "review", 0)))
				a, err = OpenActivity(root, "ticket", "review", 0)
			case "log escape", "newest escape":
				require.NoError(t, os.Symlink(outside, LogPath(root, "ticket", "review")))
				if mode == "newest escape" {
					a, err = OpenNewestLog(root, "ticket")
				} else {
					a, err = OpenActivity(root, "ticket", "review", 0)
				}
			case "explicit escape":
				a, err = OpenActivityLog(root, "ticket", "review", 0, outside)
			default:
				require.NoError(t, os.WriteFile(path, []byte("visit output"), 0o644))
				if mode == "sidecar wins visit" {
					require.NoError(t, os.WriteFile(EventsPath(root, "ticket", "review", 0), []byte(`{"events":[]}`), 0o644))
				}
				a, err = OpenActivityLog(root, "ticket", "review", 0, path)
			}
			if strings.Contains(mode, "escape") {
				require.Error(t, err)
				assert.Nil(t, a)
				return
			}
			require.NoError(t, err)
			defer a.File.Close()
			if mode == "sidecar wins visit" {
				assert.Equal(t, "events", a.Source)
			} else {
				content, _, _, err := a.Content(4)
				require.NoError(t, err)
				assert.Equal(t, "tput", content)
			}
		})
	}
}

func TestActivityContentSuffix(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		tail          int
		want          string
		start         int64
	}{
		{name: "full", content: "hello", want: "hello"},
		{name: "empty"},
		{name: "larger limit", content: "hello", tail: 10, want: "hello"},
		{name: "exact limit", content: "hello", tail: 5, want: "hello"},
		{name: "ascii suffix", content: "hello", tail: 3, want: "llo", start: 2},
		{name: "rune boundary", content: "a€xyz", tail: 6, want: "€xyz", start: 1},
		{name: "inside three byte rune", content: "a€xyz", tail: 5, want: "xyz", start: 4},
		{name: "inside four byte rune", content: "a😀z", tail: 4, want: "z", start: 5},
		{name: "no full rune fits", content: "a😀", tail: 1, start: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "id"), 0o755))
			require.NoError(t, os.WriteFile(LogPath(root, "id", "s"), []byte(tc.content), 0o644))
			a, err := OpenActivity(root, "id", "s", 0)
			require.NoError(t, err)
			defer a.File.Close()
			got, start, size, err := a.Content(tc.tail)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.start, start)
			assert.EqualValues(t, len(tc.content), size)
			assert.True(t, utf8.ValidString(got))
			if tc.tail > 0 {
				assert.LessOrEqual(t, len(got), tc.tail)
			}
		})
	}
}

func TestActivityContentShrink(t *testing.T) {
	for _, tc := range []struct {
		name, replacement, want string
		tail                    int
		start                   int64
	}{
		{name: "full", replacement: "new\n", want: "new\n"},
		{name: "preview beyond new end", replacement: "new\n", tail: 5, want: "new\n"},
		{name: "preview within new content", replacement: "replacement\n", tail: 8, want: "acement\n", start: 4},
		{name: "preview after partial read", replacement: "shorter replacement\n", tail: 8, want: "acement\n", start: 12},
		{name: "preview UTF-8 boundary", replacement: "a€xyz", tail: 5, want: "xyz", start: 4},
		{name: "empty replacement", tail: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "id"), 0o755))
			path := LogPath(root, "id", "s")
			require.NoError(t, os.WriteFile(path, []byte("previous longer output"), 0o644))
			a, err := OpenActivity(root, "id", "s", 0)
			require.NoError(t, err)
			defer a.File.Close()
			before := FileVersion(a.Path, a.Info)
			require.NoError(t, os.WriteFile(path, []byte(tc.replacement), 0o644))
			got, start, size, err := a.Content(tc.tail)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.start, start)
			assert.EqualValues(t, len(tc.replacement), size)
			assert.Equal(t, before, FileVersion(a.Path, a.Info), "retain the opening snapshot so callers reject its validator")
			current, err := a.File.Stat()
			require.NoError(t, err)
			assert.True(t, os.SameFile(a.Info, current))
			assert.NotEqual(t, before, FileVersion(a.Path, current))
		})
	}
}

func TestActivityContentReadFailure(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "id"), 0o755))
	require.NoError(t, os.WriteFile(LogPath(root, "id", "s"), []byte("content"), 0o644))
	a, err := OpenActivity(root, "id", "s", 0)
	require.NoError(t, err)
	require.NoError(t, a.File.Close())
	content, start, size, err := a.Content(3)
	require.ErrorIs(t, err, os.ErrClosed)
	assert.Empty(t, content)
	assert.Zero(t, start)
	assert.Zero(t, size)
}

func TestFileVersionReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	require.NoError(t, os.WriteFile(path, []byte("one"), 0o644))
	before, err := os.Stat(path)
	require.NoError(t, err)
	replacement := path + ".new"
	require.NoError(t, os.WriteFile(replacement, []byte("two"), 0o644))
	require.NoError(t, os.Chtimes(replacement, before.ModTime(), before.ModTime()))
	require.NoError(t, os.Rename(replacement, path))
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.NotEqual(t, FileVersion(path, before), FileVersion(path, after))
}

func BenchmarkActivitySuffix(b *testing.B) {
	root := b.TempDir()
	dir := filepath.Join(root, "id")
	require.NoError(b, os.Mkdir(dir, 0o755))
	require.NoError(b, os.WriteFile(LogPath(root, "id", "s"), []byte(strings.Repeat("x", 8<<20)), 0o644))
	for _, tail := range []int{0, 256 << 10} {
		name := "full"
		if tail > 0 {
			name = "256KiB"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				a, err := OpenActivity(root, "id", "s", 0)
				require.NoError(b, err)
				_, _, _, err = a.Content(tail)
				a.File.Close()
				require.NoError(b, err)
			}
		})
	}
}
