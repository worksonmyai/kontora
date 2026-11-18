package web

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestVisualBrowserJS(t *testing.T) {
	if os.Getenv("KONTORA_TEST_VISUAL") != "1" {
		t.Skip("run make test-visual to opt in to screenshot comparisons")
	}
	mode := os.Getenv("KONTORA_VISUAL_MODE")
	ci := os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != ""
	pinnedRenderer := runtime.GOOS == "linux" && runtime.GOARCH == "amd64" && os.Getenv("KONTORA_VISUAL_RENDERER") == "1"
	args, err := visualArgs(mode, ci, pinnedRenderer)
	if err != nil {
		t.Fatal(err)
	}
	// go test starts the binary in the package directory, even with -trimpath.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := visualCommand(t.Context(), root, args)
	if err != nil {
		t.Fatal(err)
	}
	assets := serveBrowserAssets(t)
	cmd.Env = append(cmd.Environ(), assets.env("chromium")...)
	out, err := runVisualCommand(cmd)
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	t.Logf("%s", out)
}

func visualArgs(mode string, ci, pinnedRenderer bool) ([]string, error) {
	var args []string
	target := "test-visual"
	switch mode {
	case "", "check":
		args = []string{"--update-snapshots=none"}
	case "repeat":
		target = "test-visual-repeat"
		args = []string{"--update-snapshots=none", "--repeat-each=10"}
	case "update":
		if ci {
			return nil, fmt.Errorf("visual baseline updates are disabled in CI")
		}
		target = "test-visual-update"
		args = []string{"--update-snapshots=all"}
	default:
		return nil, fmt.Errorf("unknown visual mode %q; expected check, repeat or update", mode)
	}
	if !pinnedRenderer {
		return nil, fmt.Errorf("visual tests require the pinned renderer; use make %s", target)
	}
	return args, nil
}

func visualCommand(ctx context.Context, root string, args []string) (*exec.Cmd, error) {
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("visual tests require Node.js on PATH; use make test-visual: %w", err)
	}
	testdata := filepath.Join(root, "internal", "web", "testdata")
	cli := filepath.Join(testdata, "node_modules", "playwright", "cli.js")
	if _, err := os.Stat(cli); err != nil {
		return nil, fmt.Errorf("visual tests require the locked Playwright package; use make test-visual: %w", err)
	}
	cmd := exec.CommandContext(ctx, node, append([]string{cli, "test", "--config", filepath.Join(testdata, "visual.config.mjs")}, args...)...)
	cmd.Dir = root
	return cmd, nil
}

func runVisualCommand(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("Playwright visual tests failed: %w", err)
	}
	return out, nil
}

func TestVisualArgs(t *testing.T) {
	for _, tc := range []struct {
		name, mode, wantErr string
		want                []string
		ci, pinned          bool
	}{
		{name: "default", pinned: true, want: []string{"--update-snapshots=none"}},
		{name: "ci check", mode: "check", ci: true, pinned: true, want: []string{"--update-snapshots=none"}},
		{name: "repeat", mode: "repeat", pinned: true, want: []string{"--update-snapshots=none", "--repeat-each=10"}},
		{name: "local update", mode: "update", pinned: true, want: []string{"--update-snapshots=all"}},
		{name: "ci update", mode: "update", ci: true, pinned: true, wantErr: "disabled in CI"},
		{name: "unpinned check", mode: "check", wantErr: "use make test-visual"},
		{name: "unpinned repeat", mode: "repeat", wantErr: "use make test-visual-repeat"},
		{name: "unpinned update", mode: "update", wantErr: "use make test-visual-update"},
		{name: "invalid mode", mode: "missing", wantErr: "unknown visual mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := visualArgs(tc.mode, tc.ci, tc.pinned)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(args, tc.want) {
				t.Fatalf("args = %q, error = %v; want %q", args, err, tc.want)
			}
		})
	}
}

func TestVisualDependencies(t *testing.T) {
	root := t.TempDir()
	t.Run("missing Node", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := visualCommand(t.Context(), root, nil); err == nil || !strings.Contains(err.Error(), "require Node.js") {
			t.Fatalf("expected missing Node diagnostic, got %v", err)
		}
	})
	t.Run("missing Playwright", func(t *testing.T) {
		if _, err := exec.LookPath("node"); err != nil {
			t.Skip("Node not installed")
		}
		if _, err := visualCommand(t.Context(), root, nil); err == nil || !strings.Contains(err.Error(), "require the locked Playwright package") {
			t.Fatalf("expected missing Playwright diagnostic, got %v", err)
		}
	})
}

func TestVisualRunnerProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, optIn, want string
		fails             bool
	}{
		{"opt-in disabled", "0", "--- SKIP: TestVisualBrowserJS", false},
		{"opt-in without renderer", "1", "visual tests require the pinned renderer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestVisualBrowserJS$", "-test.v")
			cmd.Env = append(cmd.Environ(), "KONTORA_TEST_VISUAL="+tc.optIn, "KONTORA_VISUAL_MODE=check", "KONTORA_VISUAL_RENDERER=")
			out, err := runVisualCommand(cmd)
			if (err != nil) != tc.fails || !strings.Contains(string(out), tc.want) {
				t.Fatalf("want failure=%t and %q, got %v:\n%s", tc.fails, tc.want, err, out)
			}
		})
	}
}
