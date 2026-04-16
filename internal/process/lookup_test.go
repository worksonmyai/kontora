package process

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLookupBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-bin")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755))

	// Seed a fallback candidate by pointing HOME at a temp dir that contains
	// ~/.local/bin/kontora-testbin. LookupBinary consults $PATH first, so we
	// use a binary name that won't collide with anything real.
	fakeHome := t.TempDir()
	fallbackBin := filepath.Join(fakeHome, ".local", "bin", "kontora-fallback-bin")
	require.NoError(t, os.MkdirAll(filepath.Dir(fallbackBin), 0o755))
	require.NoError(t, os.WriteFile(fallbackBin, []byte("#!/bin/sh\n"), 0o755))

	cases := []struct {
		name    string
		binary  string
		home    string
		wantErr bool
		want    string
	}{
		{name: "empty", binary: "", wantErr: true},
		{name: "absolute existing", binary: bin, want: bin},
		{name: "absolute missing", binary: filepath.Join(dir, "nope"), wantErr: true},
		{name: "relative not found", binary: "definitely-not-a-real-binary-xyz", wantErr: true},
		{name: "on PATH", binary: "sh", want: "/bin/sh"},
		{name: "fallback ~/.local/bin", binary: "kontora-fallback-bin", home: fakeHome, want: fallbackBin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.home != "" {
				t.Setenv("HOME", tc.home)
			}
			got, err := LookupBinary(tc.binary)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.name == "on PATH" {
				// /bin/sh is a symlink on some platforms; just require it resolves somewhere.
				assert.NotEmpty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLookupBinary_ErrorMentionsSearchedDirs(t *testing.T) {
	_, err := LookupBinary("definitely-not-a-real-binary-xyz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "$PATH")
	assert.Contains(t, err.Error(), "/usr/local/bin")
}
