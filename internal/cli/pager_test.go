package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPagerCommand(t *testing.T) {
	const unset = "\x00"

	cases := []struct {
		name     string
		kontora  string
		ticket   string
		pager    string
		cfgPager string
		want     string
	}{
		{"nothing set", unset, unset, unset, "", ""},
		{"config only", unset, unset, unset, "glow -p", "glow -p"},
		{"PAGER beats config", unset, unset, "less", "glow", "less"},
		{"TICKET_PAGER beats PAGER", unset, "glow -p", "less", "", "glow -p"},
		{"KONTORA_PAGER beats both", "bat", "glow -p", "less", "", "bat"},
		{"blank KONTORA_PAGER turns paging off", "", "glow -p", "less", "glow", ""},
		{"whitespace turns paging off", "   ", "glow -p", "less", "glow", ""},
		{"args survive whole", "glow -p -w 100", unset, unset, "", "glow -p -w 100"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every var is always handled, so an exported value cannot leak in.
			for name, v := range map[string]string{
				PagerEnvVar:       tc.kontora,
				TicketPagerEnvVar: tc.ticket,
				"PAGER":           tc.pager,
			} {
				if v == unset {
					t.Setenv(name, "")
					require.NoError(t, os.Unsetenv(name))
					continue
				}
				t.Setenv(name, v)
			}

			assert.Equal(t, tc.want, PagerCommand(tc.cfgPager))
		})
	}
}

// fakePager writes an executable shell script and returns a command line that
// runs it. Extra args prove strings.Fields passes every word through.
func fakePager(t *testing.T, body string, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pager")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return strings.Join(append([]string{path}, args...), " ")
}

func TestPage(t *testing.T) {
	// page() falls back whenever out is not a terminal, so a bytes.Buffer
	// exercises the no-pager path and an *os.File pipe is never a terminal
	// either. Both fall back, which is what the negative cases assert.
	t.Run("no pager configured writes straight through", func(t *testing.T) {
		var out, warn bytes.Buffer
		err := page(&out, &warn, "", func(w io.Writer) error {
			_, err := io.WriteString(w, "hello")
			return err
		})
		require.NoError(t, err)
		assert.Equal(t, "hello", out.String())
		assert.Empty(t, warn.String())
	})

	t.Run("not a terminal writes straight through", func(t *testing.T) {
		var out, warn bytes.Buffer
		called := false
		err := page(&out, &warn, fakePager(t, "cat >/dev/null"), func(w io.Writer) error {
			called = true
			_, err := io.WriteString(w, "hello")
			return err
		})
		require.NoError(t, err)
		assert.True(t, called)
		assert.Equal(t, "hello", out.String())
	})

	t.Run("callback error propagates", func(t *testing.T) {
		var out, warn bytes.Buffer
		want := fmt.Errorf("boom")
		err := page(&out, &warn, "", func(io.Writer) error { return want })
		assert.ErrorIs(t, err, want)
	})
}

// TestPageSpawns drives the spawning half by handing page() a real terminal
// stand-in. There is no pty here, so it calls the internals the same way page()
// does once outputWidth clears.
func TestPageSpawns(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		write    string
		wantFile string
		wantWarn string
	}{
		{
			name:     "bytes reach the pager",
			body:     `cat > "$1"`,
			write:    "paged output",
			wantFile: "paged output",
		},
		{
			name:  "pager exits non-zero",
			body:  `cat > "$1"; exit 3`,
			write: "paged output",
			// The output already arrived; the pager's status is not kontora's.
			wantFile: "paged output",
		},
		{
			name:  "pager quits before reading",
			body:  `exit 0`,
			write: strings.Repeat("x", 1<<20), // past the 64 KiB pipe buffer
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outFile := filepath.Join(t.TempDir(), "captured")
			pager := fakePager(t, tc.body, outFile)

			var warn bytes.Buffer
			err := pageSpawn(io.Discard, &warn, pager, func(w io.Writer) error {
				_, err := io.WriteString(w, tc.write)
				return err
			})
			require.NoError(t, err)

			if tc.wantFile != "" {
				got, readErr := os.ReadFile(outFile)
				require.NoError(t, readErr)
				assert.Equal(t, tc.wantFile, string(got))
			}
			assert.Equal(t, tc.wantWarn, warn.String())
		})
	}

	t.Run("missing binary falls back and warns", func(t *testing.T) {
		var out, warn bytes.Buffer
		err := pageSpawn(&out, &warn, "/nonexistent/pager", func(w io.Writer) error {
			_, err := io.WriteString(w, "hello")
			return err
		})
		require.NoError(t, err)
		assert.Equal(t, "hello", out.String())
		assert.Contains(t, warn.String(), "/nonexistent/pager")
	})
}
