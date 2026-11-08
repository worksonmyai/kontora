package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSecondarySourcesDefaultsAndYAML(t *testing.T) {
	for _, list := range []string{"", "secondary_tickets_dirs: []\n", "secondary_tickets_dirs: null\n"} {
		cfg, err := LoadReader(strings.NewReader("agents:\n  claude:\n    binary: claude\n" + list))
		require.NoError(t, err)
		assert.Empty(t, cfg.SecondaryTicketsDirs)
		sources, err := cfg.SecondarySources()
		require.NoError(t, err)
		assert.Empty(t, sources)
	}

	primary := t.TempDir()
	secondary := filepath.Join(t.TempDir(), "unavailable")
	input := fmt.Sprintf("agents:\n  claude:\n    binary: claude\ntickets_dir: %q\nsecondary_tickets_dirs: [%q]\n", primary, secondary)
	cfg, err := LoadReader(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, []string{secondary}, cfg.SecondaryTicketsDirs)
	encoded, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	again, err := LoadReader(strings.NewReader(string(encoded)))
	require.NoError(t, err)
	assert.Equal(t, cfg.SecondaryTicketsDirs, again.SecondaryTicketsDirs)
	sources, err := cfg.SecondarySources()
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.True(t, filepath.IsAbs(sources[0].Path))
	_, err = os.Stat(secondary)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestSecondarySourcesRejectAliases(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", root)
	primary := filepath.Join(root, "primary")
	secondary := filepath.Join(root, "secondary")
	for _, dir := range []string{primary, secondary} {
		require.NoError(t, os.Mkdir(dir, 0o755))
	}
	primaryLink := filepath.Join(root, "primary-link")
	secondaryLink := filepath.Join(root, "secondary-link")
	require.NoError(t, os.Symlink(primary, primaryLink))
	require.NoError(t, os.Symlink(secondary, secondaryLink))
	missing := filepath.Join(root, "missing")
	missingLink := filepath.Join(root, "missing-link")
	require.NoError(t, os.Symlink("missing", missingLink))

	for _, tc := range []struct {
		name      string
		primary   string
		secondary []string
		want      string
	}{
		{"primary exact", primary, []string{primary}, "aliases tickets_dir"},
		{"primary relative", primary, []string{"./primary"}, "aliases tickets_dir"},
		{"primary tilde", primary, []string{"~/primary"}, "aliases tickets_dir"},
		{"primary symlink", primary, []string{primaryLink}, "aliases tickets_dir"},
		{"primary is symlink", primaryLink, []string{primary}, "aliases tickets_dir"},
		{"secondary exact", primary, []string{secondary, secondary}, "aliases secondary_tickets_dirs[0]"},
		{"secondary symlink", primary, []string{secondary, secondaryLink}, "aliases secondary_tickets_dirs[0]"},
		{"missing primary", missing, []string{missing}, "aliases tickets_dir"},
		{"missing secondary", primary, []string{missing, "./missing"}, "aliases secondary_tickets_dirs[0]"},
		{"missing symlink", primary, []string{missing, missingLink}, "aliases secondary_tickets_dirs[0]"},
		{"missing symlink child", primary, []string{missing + "/child", missingLink + "/child"}, "aliases secondary_tickets_dirs[0]"},
		{"empty entry", primary, []string{""}, "directory path is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{TicketsDir: tc.primary, SecondaryTicketsDirs: tc.secondary}
			sources, err := cfg.SecondarySources()
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, sources, "invalid lists must not return partial sources")
		})
	}
}

func TestSecondarySourcesStableIdentities(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", root)
	primary := t.TempDir()
	secondary := filepath.Join(root, "secondary")
	require.NoError(t, os.Mkdir(secondary, 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink("secondary", link))
	physical, err := DirectoryIdentity(secondary)
	require.NoError(t, err)

	var first TicketSource
	for _, path := range []string{secondary, "secondary", "~/secondary", link, link + "/../secondary"} {
		cfg := &Config{TicketsDir: primary, SecondaryTicketsDirs: []string{path}}
		sources, err := cfg.SecondarySources()
		require.NoError(t, err)
		require.Len(t, sources, 1)
		assert.Equal(t, physical, sources[0].Path)
		assert.Equal(t, []string{path}, cfg.SecondaryTicketsDirs, "resolution must not rewrite YAML paths")
		if first.ID == "" {
			first = sources[0]
		} else {
			assert.Equal(t, first, sources[0])
		}
	}

	other := t.TempDir()
	cfg := &Config{TicketsDir: primary, SecondaryTicketsDirs: []string{secondary, other}}
	sources, err := cfg.SecondarySources()
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.NotEqual(t, sources[0].ID, sources[1].ID)
	cfg.SecondaryTicketsDirs = []string{other, secondary}
	reordered, err := cfg.SecondarySources()
	require.NoError(t, err)
	assert.Equal(t, sources[0], reordered[1])
	assert.Equal(t, sources[1], reordered[0])
}

func TestSecondarySourcesUnavailable(t *testing.T) {
	root := t.TempDir()
	physicalRoot, err := DirectoryIdentity(root)
	require.NoError(t, err)
	link := filepath.Join(t.TempDir(), "root-link")
	require.NoError(t, os.Symlink(root, link))
	missing := filepath.Join(link, "missing", "nested")
	file := filepath.Join(root, "regular-file")
	require.NoError(t, os.WriteFile(file, []byte("unchanged"), 0o644))
	cfg := &Config{TicketsDir: filepath.Join(root, "missing-primary"), SecondaryTicketsDirs: []string{missing, file}}
	sources, err := cfg.SecondarySources()
	require.NoError(t, err)
	require.Len(t, sources, 2)
	assert.Equal(t, filepath.Join(physicalRoot, "missing", "nested"), sources[0].Path)
	_, err = os.Stat(filepath.Join(root, "missing"))
	assert.ErrorIs(t, err, os.ErrNotExist, "source resolution must not create directories")
	_, err = os.Stat(cfg.TicketsDir)
	assert.ErrorIs(t, err, os.ErrNotExist)
	contents, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", string(contents))

	require.NoError(t, os.MkdirAll(missing, 0o755))
	available, err := cfg.SecondarySources()
	require.NoError(t, err)
	assert.Equal(t, sources, available, "creating a missing directory must not change its source ID")
}

func TestSecondarySourcesUnreadable(t *testing.T) {
	unreadable := filepath.Join(t.TempDir(), "unreadable")
	require.NoError(t, os.Mkdir(unreadable, 0o700))
	require.NoError(t, os.Chmod(unreadable, 0))
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o700) })
	if _, err := os.ReadDir(unreadable); err == nil {
		t.Skip("the current user can read directories without permission bits")
	}
	cfg := &Config{TicketsDir: t.TempDir(), SecondaryTicketsDirs: []string{unreadable}}
	sources, err := cfg.SecondarySources()
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.True(t, filepath.IsAbs(sources[0].Path))
	_, err = os.ReadDir(unreadable)
	assert.Error(t, err, "source resolution must not change permissions")
}

func TestSecondarySourcesAfterPrimaryOverrides(t *testing.T) {
	for _, env := range []string{TicketsDirEnvVar, LegacyTicketsDirEnvVar} {
		for _, initiallyAliases := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/initial-alias=%t", env, initiallyAliases), func(t *testing.T) {
				t.Setenv(TicketsDirEnvVar, "")
				t.Setenv(LegacyTicketsDirEnvVar, "")
				primary := t.TempDir()
				secondary := t.TempDir()
				filePrimary, envPrimary := primary, secondary
				if initiallyAliases {
					filePrimary, envPrimary = secondary, primary
				}
				input := fmt.Sprintf("agents:\n  claude:\n    binary: claude\ntickets_dir: %q\nsecondary_tickets_dirs: [%q]\n", filePrimary, secondary)
				cfg, err := LoadReader(strings.NewReader(input))
				require.NoError(t, err, "loading must defer alias validation until after overrides")
				t.Setenv(env, envPrimary)
				assert.Equal(t, env, cfg.ApplyEnvOverrides())
				_, err = cfg.SecondarySources()
				if initiallyAliases {
					require.NoError(t, err, "the environment removed the alias")
				} else {
					require.ErrorContains(t, err, "aliases tickets_dir")
				}

				cfg.SetTicketsDir(secondary)
				_, err = cfg.SecondarySources()
				require.ErrorContains(t, err, "aliases tickets_dir", "the command-line override wins")
				cfg.SetTicketsDir(primary)
				_, err = cfg.SecondarySources()
				require.NoError(t, err)
			})
		}
	}
}

func TestSecondarySourcesPrimaryEnvironmentPrecedence(t *testing.T) {
	primary, secondary := t.TempDir(), t.TempDir()
	cfg := &Config{TicketsDir: secondary, SecondaryTicketsDirs: []string{secondary}}
	t.Setenv(LegacyTicketsDirEnvVar, secondary)
	t.Setenv(TicketsDirEnvVar, primary)
	assert.Equal(t, TicketsDirEnvVar, cfg.ApplyEnvOverrides())
	_, err := cfg.SecondarySources()
	require.NoError(t, err, "KONTORA_TICKETS_DIR must beat TICKETS_DIR")
	cfg.SetTicketsDir(secondary)
	_, err = cfg.SecondarySources()
	require.ErrorContains(t, err, "aliases tickets_dir", "the flag must beat both environment variables")
}

func TestSecondarySourcesSymlinkParentTraversal(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "real", "child")
	require.NoError(t, os.MkdirAll(child, 0o755))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(child, link))
	primary := filepath.Join(root, "real")
	cfg := &Config{TicketsDir: primary, SecondaryTicketsDirs: []string{link + "/.."}}
	_, err := cfg.SecondarySources()
	require.ErrorContains(t, err, "aliases tickets_dir")

	cfg.SecondaryTicketsDirs = []string{link + "/../missing", filepath.Join(primary, "missing")}
	_, err = cfg.SecondarySources()
	require.ErrorContains(t, err, "aliases secondary_tickets_dirs[0]")
}
