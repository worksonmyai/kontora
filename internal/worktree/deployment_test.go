package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func deploymentTestGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := gitOutput(repo, args...)
	require.NoError(t, err)
	return out
}

func TestDeploymentMatches(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	first := revParse(t, repo, "HEAD")
	second := exactCommitFile(t, repo, "second", "second")
	third := exactCommitFile(t, repo, "third", "third")
	// An orphan commit with identical content is not ancestry evidence.
	unrelated := deploymentTestGit(t, repo, "commit-tree", revParse(t, repo, "HEAD^{tree}"), "-m", "unrelated")
	repository, frozen, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	require.Equal(t, third, frozen)
	for _, tc := range []struct {
		name, required, deployed string
		want                     bool
	}{
		{"equal", first, first, true},
		{"descendant", first, third, true},
		{"immediate descendant", second, third, true},
		{"older deployment", third, first, false},
		{"unrelated", first, unrelated, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeploymentMatches(repo, repository, tc.required, tc.deployed)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestDeploymentEvidenceCanonicalIdentityAndFrozenHEAD(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	first := revParse(t, repo, "HEAD")
	wt := filepath.Join(t.TempDir(), "linked worktree")
	managedGit(t, repo, "worktree", "add", "--detach", wt, first)
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(repo, alias))
	repository, frozen, err := DeploymentEvidence(alias, wt)
	require.NoError(t, err)
	common, err := CommonDir(repo)
	require.NoError(t, err)
	require.Equal(t, common, repository)
	require.Equal(t, first, frozen)
	second := exactCommitFile(t, wt, "next", "next")
	require.NotEqual(t, first, second)
	got, err := DeploymentMatches(alias, repository, frozen, second)
	require.NoError(t, err)
	require.True(t, got)
	// Identity is canonical, not a path alias or a working-directory path.
	for _, identity := range []string{repo, filepath.Join(alias, ".git"), "", common + string(os.PathSeparator)} {
		got, err := DeploymentMatches(repo, identity, first, second)
		require.NoError(t, err)
		require.False(t, got)
	}
	other := initRepo(t)
	got, err = DeploymentMatches(other, repository, first, second)
	require.NoError(t, err)
	require.False(t, got)
	identity, oid, err := DeploymentEvidence(repo, other)
	require.Error(t, err)
	require.Empty(t, identity)
	require.Empty(t, oid)
}

func TestDeploymentRejectsNonCommitInputs(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	oid := exactCommitFile(t, repo, "README.md", "fixture")
	managedGit(t, repo, "tag", "-a", "annotated", "-m", "annotated")
	repository, _, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	for _, invalid := range []string{
		"", "HEAD", "refs/heads/main", "annotated", oid[:12], oid + "^", oid + "^{commit}",
		strings.ToUpper(oid), "--all", strings.Repeat("g", 40), strings.Repeat("0", 40),
		revParse(t, repo, "HEAD^{tree}"), revParse(t, repo, "HEAD:README.md"),
		revParse(t, repo, "annotated"),
	} {
		t.Run(invalid, func(t *testing.T) {
			for _, pair := range [][2]string{{invalid, oid}, {oid, invalid}} {
				got, err := DeploymentMatches(repo, repository, pair[0], pair[1])
				require.Error(t, err)
				require.False(t, got)
			}
		})
	}
}

func TestDeploymentIgnoresReplacementsAndGrafts(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	first := revParse(t, repo, "HEAD")
	second := exactCommitFile(t, repo, "second", "second")
	orphan := deploymentTestGit(t, repo, "commit-tree", revParse(t, repo, "HEAD^{tree}"), "-m", "orphan")
	repository, _, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	// Ordinary Git now claims orphan descends from first; evidence must not.
	managedGit(t, repo, "replace", orphan, second)
	managedGit(t, repo, "merge-base", "--is-ancestor", first, orphan)
	got, err := DeploymentMatches(repo, repository, first, orphan)
	require.NoError(t, err)
	require.False(t, got)
	managedGit(t, repo, "replace", "-d", orphan)
	require.NoError(t, os.WriteFile(filepath.Join(repository, "info", "grafts"), []byte(orphan+" "+first+"\n"), 0o600))
	got, err = DeploymentMatches(repo, repository, first, orphan)
	require.NoError(t, err)
	require.False(t, got)
	// A replacement that hides real ancestry must not hide a valid match.
	require.NoError(t, os.Remove(filepath.Join(repository, "info", "grafts")))
	managedGit(t, repo, "replace", second, orphan)
	got, err = DeploymentMatches(repo, repository, first, second)
	require.NoError(t, err)
	require.True(t, got)
	// Replacement refs must not turn a tree into acceptable commit evidence.
	tree := revParse(t, repo, "HEAD^{tree}")
	managedGit(t, repo, "replace", "-f", tree, first)
	got, err = DeploymentMatches(repo, repository, tree, first)
	require.Error(t, err)
	require.False(t, got)
}

func TestDeploymentMissingHistory(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"required", "deployed", "parent"} {
		t.Run(missing, func(t *testing.T) {
			repo := initRepo(t)
			parent := revParse(t, repo, "HEAD")
			required := exactCommitFile(t, repo, "required", "required")
			deployed := exactCommitFile(t, repo, "deployed", "deployed")
			repository, _, err := DeploymentEvidence(repo, repo)
			require.NoError(t, err)
			// Commit graphs must not substitute for missing objects.
			managedGit(t, repo, "commit-graph", "write", "--reachable")
			oid := map[string]string{"required": required, "deployed": deployed, "parent": parent}[missing]
			managedGit(t, repo, "replace", oid, deployed)
			require.NoError(t, os.Remove(filepath.Join(repository, "objects", oid[:2], oid[2:])))
			got, err := DeploymentMatches(repo, repository, required, deployed)
			require.Error(t, err)
			require.False(t, got)
			if missing == "deployed" {
				identity, commit, err := DeploymentEvidence(repo, repo)
				require.Error(t, err)
				require.Empty(t, identity)
				require.Empty(t, commit)
			}
		})
	}
}

func TestDeploymentShallowHistory(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	first := revParse(t, repo, "HEAD")
	second := exactCommitFile(t, repo, "second", "second")
	third := exactCommitFile(t, repo, "third", "third")
	shallow := filepath.Join(t.TempDir(), "shallow")
	managedGit(t, repo, "clone", "--depth=2", "file://"+repo, shallow)
	// Keep the older object locally without removing the shallow boundary.
	common, err := CommonDir(repo)
	require.NoError(t, err)
	source, err := os.ReadFile(filepath.Join(common, "objects", first[:2], first[2:]))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(shallow, ".git", "objects", first[:2]), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(shallow, ".git", "objects", first[:2], first[2:]), source, 0o600))
	repository, frozen, err := DeploymentEvidence(shallow, shallow)
	require.NoError(t, err)
	require.Equal(t, third, frozen)
	for _, pair := range [][2]string{{first, third}, {second, third}, {third, first}} {
		got, err := DeploymentMatches(shallow, repository, pair[0], pair[1])
		require.Error(t, err)
		require.False(t, got)
	}
	got, err := DeploymentMatches(shallow, repository, third, third)
	require.NoError(t, err)
	require.True(t, got)
}

func TestDeploymentMissingPromisorObjectDoesNotFetch(t *testing.T) {
	t.Parallel()
	origin := initRepo(t)
	first := revParse(t, origin, "HEAD")
	second := exactCommitFile(t, origin, "next", "next")
	repo := filepath.Join(t.TempDir(), "clone")
	managedGit(t, origin, "clone", "--no-hardlinks", origin, repo)
	repository, _, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "fetch-attempted")
	uploadPack := filepath.Join(t.TempDir(), "upload-pack")
	script := fmt.Sprintf("#!/bin/sh\nprintf attempted > %q\nexit 1\n", marker)
	require.NoError(t, os.WriteFile(uploadPack, []byte(script), 0o700))
	managedGit(t, repo, "config", "remote.origin.promisor", "true")
	managedGit(t, repo, "config", "remote.origin.partialclonefilter", "blob:none")
	managedGit(t, repo, "config", "remote.origin.uploadpack", uploadPack)
	managedGit(t, repo, "config", "extensions.partialClone", "origin")
	managedGit(t, repo, "replace", first, second)
	object := filepath.Join(repository, "objects", first[:2], first[2:])
	require.NoError(t, os.Remove(object))
	got, err := DeploymentMatches(repo, repository, first, second)
	require.Error(t, err)
	require.False(t, got)
	require.NoFileExists(t, marker)
	require.NoFileExists(t, object)
}

func TestDeploymentSHA256(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	managedGit(t, repo, "init", "--object-format=sha256")
	managedGit(t, repo, "config", "user.name", "Test")
	managedGit(t, repo, "config", "user.email", "test@test.com")
	managedGit(t, repo, "config", "commit.gpgsign", "false")
	first := exactCommitFile(t, repo, "first", "first")
	second := exactCommitFile(t, repo, "second", "second")
	repository, frozen, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	require.Len(t, frozen, 64)
	require.Equal(t, second, frozen)
	got, err := DeploymentMatches(repo, repository, first, second)
	require.NoError(t, err)
	require.True(t, got)
	got, err = DeploymentMatches(repo, repository, first[:40], second)
	require.Error(t, err)
	require.False(t, got)
}

func TestDeploymentInvalidWorktrees(t *testing.T) {
	t.Parallel()
	repo := initRepo(t)
	bare := filepath.Join(t.TempDir(), "bare")
	managedGit(t, repo, "clone", "--bare", repo, bare)
	unborn := t.TempDir()
	managedGit(t, unborn, "init")
	for _, pair := range [][2]string{{"", repo}, {repo, ""}, {repo, t.TempDir()}, {unborn, unborn}, {bare, bare}} {
		identity, commit, err := DeploymentEvidence(pair[0], pair[1])
		require.Error(t, err)
		require.Empty(t, identity)
		require.Empty(t, commit)
	}
}

func TestDeploymentRestrictedGit(t *testing.T) {
	// Environment changes intentionally keep this test non-parallel.
	repo := initRepo(t)
	first := revParse(t, repo, "HEAD")
	second := exactCommitFile(t, repo, "next", "next")
	repository, _, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := fmt.Sprintf(`#!/bin/sh
printf 'call\n' >> %q
[ "$1" = '--no-lazy-fetch' ] || exit 90
[ "$2" = '--no-replace-objects' ] || exit 91
[ -z "$GIT_DIR$GIT_WORK_TREE$GIT_OBJECT_DIRECTORY$GIT_ALTERNATE_OBJECT_DIRECTORIES$GIT_SHALLOW_FILE$GIT_CONFIG_COUNT$GIT_CONFIG_PARAMETERS" ] || exit 92
exec %q "$@"
`, log, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		t.Setenv(name, "invalid inherited override")
	}
	identity, commit, err := DeploymentEvidence(repo, repo)
	require.NoError(t, err)
	require.Equal(t, repository, identity)
	require.Equal(t, second, commit)
	got, err := DeploymentMatches(repo, repository, first, second)
	require.NoError(t, err)
	require.True(t, got)
	// Simulate a Git version that lacks either safety option. No fallback.
	for _, option := range []string{"--no-lazy-fetch", "--no-replace-objects"} {
		script = fmt.Sprintf("#!/bin/sh\nprintf 'call\\n' >> %q\necho 'unknown option: %s' >&2\nexit 129\n", log, option)
		require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700))
		for _, evidence := range []bool{false, true} {
			require.NoError(t, os.WriteFile(log, nil, 0o600))
			if evidence {
				_, _, err = DeploymentEvidence(repo, repo)
			} else {
				got, err = DeploymentMatches(repo, repository, first, second)
				require.False(t, got)
			}
			require.ErrorContains(t, err, option)
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			require.Equal(t, "call\n", string(calls))
		}
	}
}
