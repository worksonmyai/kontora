package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestRemoteDispatch_DeleteWithConfirmation(t *testing.T) {
	var deletePath, confirmHeader, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123":
			deletePath = r.URL.Path
			method = r.Method
			confirmHeader = r.Header.Get("X-Kontora-Confirm")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "delete", "abc", "-f")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123", deletePath)
	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, "delete-ticket-file", confirmHeader)
}

func TestRemoteDispatch_DeleteWithoutConfirmationFails(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "delete", "abc")
	require.Error(t, err)
	assert.Contains(t, out, "confirmation")
	assert.False(t, hit, "no request should be sent without confirmation")
}

func TestRemoteDispatch_ExtraPositionalRejected(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A stray second positional must fail clearly rather than silently
	// swallowing the trailing flag and dying with a misleading error.
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "delete", "abc", "def", "-f")
	require.Error(t, err)
	assert.Contains(t, out, "unexpected argument")
	assert.False(t, hit, "no request should be sent for malformed input")
}

func TestRemoteDispatch_UpdateSelectedFields(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123" && r.Method == http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Flags after the ID, and an explicit empty --agent that should clear.
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "update", "abc", "--pipeline", "two-stage", "--agent", "")
	require.NoError(t, err, out)

	// Only the supplied fields are present in the JSON body.
	require.NotNil(t, gotBody)
	assert.Equal(t, "two-stage", gotBody["pipeline"])
	agent, ok := gotBody["agent"]
	assert.True(t, ok, "agent must be present (cleared with empty string)")
	assert.Equal(t, "", agent)
	_, hasPath := gotBody["path"]
	assert.False(t, hasPath, "path must be omitted when not passed")
	_, hasBody := gotBody["body"]
	assert.False(t, hasBody, "body must be omitted when not passed")
	_, hasBranch := gotBody["branch"]
	assert.False(t, hasBranch, "branch must be omitted when not passed")
}

func TestRemoteDispatch_InitWithFlags(t *testing.T) {
	var initPath string
	var gotReq map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		case r.URL.Path == "/api/tickets/abc123/init":
			initPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotReq)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "abc123"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "init", "abc", "--pipeline", "two-stage", "--path", "/repo")
	require.NoError(t, err, out)
	assert.Equal(t, "/api/tickets/abc123/init", initPath)
	assert.Equal(t, "two-stage", gotReq["pipeline"])
	assert.Equal(t, "/repo", gotReq["path"])
}

func TestRemoteDispatch_InitMissingFlagsFails(t *testing.T) {
	var initHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/init") {
			initHit = true
		}
		switch {
		case r.URL.Path == "/api/tickets" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tickets":        []map[string]string{{"id": "abc123"}},
				"running_agents": 0,
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL}, "init", "abc")
	require.Error(t, err)
	assert.Contains(t, out, "requires --pipeline and --path")
	assert.False(t, initHit, "daemon init endpoint must not be called")
}

func TestRemoteDispatch_ConfigEditSavesValid(t *testing.T) {
	var putContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": "max_concurrent_agents: 1\nagents:\n  claude:\n    binary: claude\n",
			})
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodPut:
			var body struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			putContent = body.Content
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	editor := writeFakeEditor(t, "max_concurrent_agents: 9\nagents:\n  claude:\n    binary: claude\n")
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "EDITOR=" + editor}, "config", "edit")
	require.NoError(t, err, out)
	assert.Contains(t, putContent, "max_concurrent_agents: 9")
	assert.Contains(t, out, "Restart the daemon")
}

func TestRemoteDispatch_ConfigEditInvalidNotSaved(t *testing.T) {
	var putHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": "agents:\n  claude:\n    binary: claude\n",
			})
		case r.URL.Path == "/api/config/raw" && r.Method == http.MethodPut:
			putHit = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Editor writes a config with an unknown field, which fails local validation.
	editor := writeFakeEditor(t, "totally_unknown_field: 1\n")
	out, err := runCLI(t, []string{"KONTORA_URL=" + srv.URL, "EDITOR=" + editor}, "config", "edit")
	require.Error(t, err)
	assert.Contains(t, out, "invalid")
	assert.False(t, putHit, "invalid config must not be uploaded")
}

// writeFakeEditor creates an executable script that overwrites the file passed
// as its first argument with content, simulating a user editing in $EDITOR.
func writeFakeEditor(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-editor.sh")
	script := "#!/bin/sh\ncat > \"$1\" <<'KONTORA_EOF'\n" + content + "KONTORA_EOF\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestRemoteDispatch_LocalOnlyVerbsRejected(t *testing.T) {
	for _, verb := range []string{"edit", "archive", "doctor", "fmt", "completion", "start"} {
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
