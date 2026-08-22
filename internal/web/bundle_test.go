package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain drops KONTORA_WEB_DIR from the environment. Left set, it would
// point every test that serves /app.js at whatever working copy the developer
// last exported, so the tests covering the embedded UI would either fail on an
// unrelated directory or quietly pass on another checkout's JS.
func TestMain(m *testing.M) {
	if err := os.Unsetenv(webDirEnv); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestUIBundle covers what the page and the node suite depend on: the embedded
// sources compile, the result is a classic script rather than an ES module, and
// the three values reached from outside the IIFE are assigned to the global.
func TestUIBundle(t *testing.T) {
	bundle, err := embeddedUIBundle()
	require.NoError(t, err)
	js := string(bundle)

	for _, global := range []string{"globalThis.kontora =", "globalThis.termState =", "globalThis.statsDerive ="} {
		assert.Contains(t, js, global)
	}
	for line := range strings.SplitSeq(js, "\n") {
		assert.False(t, strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "export "),
			"the bundle must be an IIFE, not an ES module: %q", line)
	}
}

func TestBuildUIBundleErrors(t *testing.T) {
	cases := []struct {
		name    string
		sources fstest.MapFS
		want    []string
	}{
		{
			name: "syntax error names the file and line",
			sources: fstest.MapFS{
				"index.js": &fstest.MapFile{Data: []byte("const a = 1;\nfunction (\n")},
			},
			want: []string{"index.js", ":2:"},
		},
		{
			name: "a bare import is rejected",
			sources: fstest.MapFS{
				"index.js": &fstest.MapFile{Data: []byte("import 'alpinejs';\n")},
			},
			want: []string{"alpinejs", "not a relative path"},
		},
		{
			name: "a missing module is rejected",
			sources: fstest.MapFS{
				"index.js": &fstest.MapFile{Data: []byte("import './gone.js';\n")},
			},
			want: []string{"no web UI module at", "gone.js"},
		},
		{
			// esbuild reports this as a warning and ships the later
			// definition, which is how a mixin loses a method to a copy-paste.
			name: "a duplicate object key fails the build",
			sources: fstest.MapFS{
				"index.js": &fstest.MapFile{Data: []byte("globalThis.kontora = () => ({ a: 1, b: 2, a: 3 });\n")},
			},
			want: []string{"Duplicate key", `"a"`},
		},
		{
			// Under FormatIIFE esbuild turns this into a __require() call that
			// throws in the browser before the bundle assigns its globals.
			name: "a static import of a vendored module is rejected",
			sources: fstest.MapFS{
				"index.js": &fstest.MapFile{Data: []byte("import { Terminal } from '/vendor/xterm@5.5.0/xterm.mjs';\nglobalThis.t = Terminal;\n")},
			},
			want: []string{"xterm.mjs", "must be reached through import()"},
		},
		{
			name: "a vendored module that is not embedded is rejected",
			sources: fstest.MapFS{
				"index.js": &fstest.MapFile{Data: []byte("globalThis.load = () => import('/vendor/xterm@9.9.9/xterm.mjs');\n")},
			},
			want: []string{"no vendored asset at", "xterm@9.9.9"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildUIBundle(tc.sources)
			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

// TestUIBundleIgnoresWorkingDirectory pins the entry-point resolve. esbuild
// rewrites a bare entry to "./index.js" when the process working directory
// holds a file of that name, and fs.FS rejects that spelling: a daemon started
// from any JS project root would then fail to build its own UI.
func TestUIBundleIgnoresWorkingDirectory(t *testing.T) {
	decoy := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(decoy, uiEntry), []byte("throw new Error('decoy');\n"), 0o600))
	t.Chdir(decoy)

	bundle, err := buildUIBundle(fstest.MapFS{
		uiEntry: &fstest.MapFile{Data: []byte("globalThis.kontora = () => ({});\n")},
	})
	require.NoError(t, err)
	assert.NotContains(t, string(bundle), "decoy")
}

// TestUIBundleWebDirOverride covers the dev path: KONTORA_WEB_DIR reads that
// directory and nothing else, so a module deleted from the working copy stays
// deleted instead of falling back to the embedded one.
func TestUIBundleWebDirOverride(t *testing.T) {
	dir := t.TempDir()
	ui := filepath.Join(dir, "internal", "web", "ui")
	require.NoError(t, os.MkdirAll(ui, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ui, "index.js"),
		[]byte("globalThis.kontora = () => ({ from: 'the working copy' });\n"), 0o600))

	t.Setenv(webDirEnv, dir)
	bundle, err := uiBundle()
	require.NoError(t, err)
	assert.Contains(t, string(bundle), "the working copy")

	t.Setenv(webDirEnv, t.TempDir())
	_, err = uiBundle()
	require.Error(t, err)
	assert.Contains(t, err.Error(), webDirEnv+"=")
	assert.Contains(t, err.Error(), "index.js")
}
