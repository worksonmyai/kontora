package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildKontora compiles the CLI once for the subprocess-based remote tests.
func buildKontora(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "kontora-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "kontora")
		out, err := exec.Command("go", "build", "-o", builtBin, ".").CombinedOutput()
		if err != nil {
			buildErr = err
			builtBin = string(out)
		}
	})
	require.NoError(t, buildErr, builtBin)
	return builtBin
}

// runCLI runs the built binary with the given args and environment.
func runCLI(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	bin := buildKontora(t)
	cmd := exec.Command(bin, args...)
	// A fresh HOME guarantees there is no local config file to fall back on.
	cmd.Env = append(os.Environ(), append([]string{"HOME=" + t.TempDir()}, env...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestRemoteDispatch_RunWithoutLocalConfig(t *testing.T) {
	var runPath, authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123/run":
			runPath = r.URL.Path
			authHeader = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "KONTORA_TOKEN=secret"}, "run", "abc")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123/run", runPath)
	assert.Equal(t, "Bearer secret", authHeader)
}

func TestRemoteDispatch_PauseSendsExactID(t *testing.T) {
	var pausePath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123def"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123def/pause":
			pausePath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123def"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "pause", "abc")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123def/pause", pausePath)
}

func TestRemoteDispatch_NoteAppends(t *testing.T) {
	var notePath, noteText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123/note":
			notePath = r.URL.Path
			var body struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			noteText = body.Text
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "note", "abc", "blocked on review")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123/note", notePath)
	assert.Equal(t, "blocked on review", noteText)
}

func TestRemoteDispatch_ServerValidationErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		default:
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid state transition"})
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "run", "abc")
	require.Error(t, err)
	assert.Contains(t, out, "invalid state transition")
}

func TestRemoteDispatch_LocalOnlyVerbsRejected(t *testing.T) {
	for _, verb := range []string{"edit", "init", "archive", "doctor", "fmt", "completion", "start"} {
		t.Run(verb, func(t *testing.T) {
			args := []string{verb}
			if verb != "fmt" && verb != "completion" && verb != "start" {
				args = append(args, "abc")
			}
			out, err := runCLI(t, []string{"KONTORA_URL=http://127.0.0.1:1"}, args...)
			require.Error(t, err, out)
			assert.Contains(t, out, "not available in remote mode")
		})
	}
}
