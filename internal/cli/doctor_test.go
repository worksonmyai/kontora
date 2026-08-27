package cli

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

// noTicketsDirEnv keeps a developer's exported TICKETS_DIR out of Doctor, which
// folds the environment over the config the way every CLI verb does.
func noTicketsDirEnv(t *testing.T) {
	t.Helper()
	t.Setenv(config.TicketsDirEnvVar, "")
	t.Setenv(config.LegacyTicketsDirEnvVar, "")
}

func writeValidConfig(t *testing.T, dir string) string {
	t.Helper()
	configPath := filepath.Join(dir, "config.yaml")
	content := `agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))
	return configPath
}

// holdDaemonLock takes the flock a running daemon holds beside configPath, for
// the rest of the test. flock is per open file description, so a lock this
// process takes on its own fd still blocks the probe's.
func holdDaemonLock(t *testing.T, configPath string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(filepath.Dir(configPath), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)
	require.NoError(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	})
}

func TestDoctor_ValidConfig(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := writeValidConfig(t, dir)

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "Config")
	assert.Contains(t, out, "git")
	assert.Contains(t, out, "tmux")
	assert.Contains(t, out, "All checks passed")
}

func TestDoctor_Daemon(t *testing.T) {
	cases := []struct {
		name string
		// lock is what sits beside the config: "" none, "stale" a file nothing
		// locks, "held" a file another open description holds, "unreadable"
		// something that is not a file at all.
		lock string
		want string
	}{
		{name: "no lock file means no daemon", want: "not running"},
		{name: "a lock nothing holds is left over from a kill", lock: "stale", want: "not running"},
		{name: "a held lock means a daemon is running", lock: "held", want: "running"},
		{name: "a lock that cannot be opened is reported as read", lock: "unreadable", want: "could not read the lock file"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noTicketsDirEnv(t)
			dir := t.TempDir()
			configPath := writeValidConfig(t, dir)
			lockPath := filepath.Join(dir, "lock")

			switch tc.lock {
			case "stale":
				require.NoError(t, os.WriteFile(lockPath, nil, 0o644))
			case "held":
				holdDaemonLock(t, configPath)
			case "unreadable":
				// Opening a directory O_RDWR fails, which is the branch that
				// reports the error rather than an answer.
				require.NoError(t, os.Mkdir(lockPath, 0o755))
			}

			var buf bytes.Buffer
			require.NoError(t, Doctor(configPath, &buf))

			assert.Regexp(t, `Daemon\s+`+tc.want, buf.String())
			if tc.want == "not running" {
				assert.Contains(t, buf.String(), "kontora start")
			}
			if tc.lock == "stale" || tc.lock == "held" {
				assert.FileExists(t, lockPath, "the probe must not remove the lock file")
			}
		})
	}
}

func TestDoctor_PipelineDefaults(t *testing.T) {
	const warning = "runs one agent on its description rather than a pipeline"

	cases := []struct {
		name     string
		extra    string
		wantWarn bool
	}{
		{name: "no projects and no default_pipeline warns", wantWarn: true},
		{name: "a default_pipeline silences it", extra: "default_pipeline: p\n"},
		{name: "a project that sets a pipeline silences it", extra: "projects:\n  demo:\n    path: %REPO%\n    pipeline: p\n"},
		{name: "a project without a pipeline still warns", extra: "projects:\n  demo:\n    path: %REPO%\n", wantWarn: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noTicketsDirEnv(t)
			dir := t.TempDir()
			configPath := writeValidConfig(t, dir)
			extra := strings.ReplaceAll(tc.extra, "%REPO%", initTestRepo(t))

			content, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(configPath, append(content, []byte(extra)...), 0o644))

			var buf bytes.Buffer
			require.NoError(t, Doctor(configPath, &buf))

			if tc.wantWarn {
				assert.Contains(t, buf.String(), warning)
				return
			}
			assert.NotContains(t, buf.String(), warning)
		})
	}
}

func TestDoctor_ConfigMissing(t *testing.T) {
	var buf bytes.Buffer
	err := Doctor("/nonexistent/path/config.yaml", &buf)
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "Config")
	assert.Contains(t, out, "Some checks failed")
}

func TestDoctor_ConfigInvalid(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("invalid: {{{"), 0o644))

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "Config")
	assert.Contains(t, out, "Some checks failed")
}

func TestDoctor_DirMissing(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(`tickets_dir: %s/nonexistent/tickets
logs_dir: %s/nonexistent/logs

agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`, dir, dir)
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "will be auto-created")
}

func TestDoctor_AgentBinaryMissing(t *testing.T) {
	noTicketsDirEnv(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := `agents:
  myagent:
    binary: nonexistent-binary-abc123

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: myagent
      on_success: done
      on_failure: pause
`
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	err := Doctor(configPath, &buf)
	require.Error(t, err)

	out := buf.String()
	assert.Contains(t, out, "nonexistent-binary-abc123")
	assert.Contains(t, out, "Some checks failed")
}

func TestDoctor_WebPortBound(t *testing.T) {
	cases := []struct {
		name          string
		daemonRunning bool
		serveHealth   bool
		wantSymbol    string
		want          string
	}{
		{name: "a port nothing explains is a warning", wantSymbol: "!", want: "taken by another process"},
		{
			name:          "a daemon that lost the port still warns",
			daemonRunning: true,
			wantSymbol:    "!",
			want:          "so the running daemon has no dashboard",
		},
		{
			name:        "a port the dashboard answers on is fine",
			serveHealth: true,
			wantSymbol:  "✓",
			want:        "serving the kontora dashboard",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noTicketsDirEnv(t)
			// Bind a port.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer ln.Close()

			port := ln.Addr().(*net.TCPAddr).Port
			if tc.serveHealth {
				// The daemon's own /health: 200 and nothing else.
				srv := &http.Server{
					ReadHeaderTimeout: time.Second,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/health" {
							http.NotFound(w, r)
							return
						}
						w.WriteHeader(http.StatusOK)
					}),
				}
				go func() { _ = srv.Serve(ln) }()
				t.Cleanup(func() { srv.Close() })
			}

			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			content := fmt.Sprintf(`web:
  enabled: true
  host: 127.0.0.1
  port: %d

agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`, port)
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))
			if tc.daemonRunning {
				holdDaemonLock(t, configPath)
			}

			var buf bytes.Buffer
			// Port bound is a warning, not a failure.
			require.NoError(t, Doctor(configPath, &buf))

			out := buf.String()
			assert.Regexp(t, tc.wantSymbol+` Web port\s+.*`+regexp.QuoteMeta(tc.want), out)
		})
	}
}

func TestDoctor_ProjectPaths(t *testing.T) {
	noTicketsDirEnv(t)
	repo := initTestRepo(t)
	notARepo := t.TempDir()

	cases := []struct {
		name     string
		path     string
		wantFail bool
		want     string
	}{
		{name: "a git repository passes", path: repo, want: repo},
		{name: "a directory that is not a repository fails", path: notARepo, wantFail: true, want: "not a git repository"},
		{name: "a missing path fails", path: filepath.Join(notARepo, "gone"), wantFail: true, want: "does not exist"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			content := fmt.Sprintf(`agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause

projects:
  demo:
    path: %s
`, tc.path)
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

			var buf bytes.Buffer
			err := Doctor(configPath, &buf)
			if tc.wantFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Contains(t, buf.String(), tc.want)
		})
	}
}

func TestDoctor_WarningsAreCounted(t *testing.T) {
	noTicketsDirEnv(t)
	// A missing directory is a warning, so the closing line must not claim a
	// clean bill of health.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(`tickets_dir: %s/nonexistent/tickets

agents:
  true:
    binary: "true"

stages:
  s:
    prompt: do stuff

pipelines:
  p:
    - stage: s
      agent: "true"
      on_success: done
      on_failure: pause
`, dir)
	require.NoError(t, os.WriteFile(configPath, []byte(content), 0o644))

	var buf bytes.Buffer
	require.NoError(t, Doctor(configPath, &buf))
	assert.Regexp(t, `All checks passed, with \d+ warning`, buf.String())
}

func TestDoctor_ReportsTicketsDirSource(t *testing.T) {
	envDir := t.TempDir()

	cases := []struct {
		name       string
		env        string
		wantSource bool
	}{
		{name: "config value stands", env: ""},
		{name: "environment overrides the config", env: envDir, wantSource: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.TicketsDirEnvVar, tc.env)
			t.Setenv(config.LegacyTicketsDirEnvVar, "")

			configPath := writeValidConfig(t, t.TempDir())

			var buf bytes.Buffer
			require.NoError(t, Doctor(configPath, &buf))
			out := buf.String()

			if !tc.wantSource {
				assert.NotContains(t, out, config.TicketsDirEnvVar)
				return
			}
			assert.Contains(t, out, envDir)
			assert.Contains(t, out, config.TicketsDirEnvVar)
			assert.Contains(t, out, "Tickets dir conflict")
			assert.Regexp(t, `All checks passed, with \d+ warning`, out)
		})
	}
}
