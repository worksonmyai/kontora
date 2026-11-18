package tmux

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPaneLogScan(t *testing.T, patterns ...string) *paneLogScan {
	t.Helper()
	scan, err := newPaneLogScan(patterns)
	require.NoError(t, err)
	t.Cleanup(scan.close)
	return scan
}

func TestDrainPaneLogFailurePatterns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing int64
		input    string
		patterns []string
		want     *string
		chunked  bool
	}{
		{name: "late provider error", input: strings.Repeat("x", maxPaneLogBytes+4096) + "provider error", patterns: []string{"provider error"}, want: new("provider error")},
		{name: "already capped", existing: maxPaneLogBytes, input: "provider error", patterns: []string{"provider error"}, want: new("provider error")},
		{name: "already oversized", existing: 45_778_216_576, input: "provider error", patterns: []string{"provider error"}, want: new("provider error")},
		{name: "empty pattern", existing: maxPaneLogBytes, input: "output", patterns: []string{""}, want: new("")},
		{name: "empty pattern takes precedence", input: "provider error", patterns: []string{"", "provider error"}, want: new("")},
		{name: "cross chunk and newline", input: "prefix\nprovider\nfailed\nsuffix", patterns: []string{`(?m)^provider\nfailed$`}, want: new(`(?m)^provider\nfailed$`), chunked: true},
		{name: "arbitrary match length", input: "provider" + strings.Repeat("x\n", 1<<16) + "failed", patterns: []string{`(?s)provider.*failed`}, want: new(`(?s)provider.*failed`)},
		{name: "configured precedence", input: "second error\nfirst error", patterns: []string{"first error", "second error"}, want: new("first error"), chunked: true},
		{name: "earlier pattern absent", input: "second error", patterns: []string{"first error", "second error"}, want: new("second error")},
		{name: "no match", input: "clean output", patterns: []string{"provider error"}},
		{name: "empty output", patterns: []string{"provider error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scan := testPaneLogScan(t, tc.patterns...)
			path := filepath.Join(t.TempDir(), "raw.log")
			if tc.existing > 0 {
				seedPaneLog(t, path, tc.existing)
			}
			var input io.Reader = strings.NewReader(tc.input)
			if tc.chunked {
				input = iotest.OneByteReader(input)
			}
			require.NoError(t, DrainPaneLog(input, path, scan.path))
			pattern, err := scan.wait(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.want, pattern)
			info, err := os.Stat(path)
			require.NoError(t, err)
			assert.EqualValues(t, min(maxPaneLogBytes, tc.existing+int64(len(tc.input))), info.Size())
		})
	}
}

func TestDrainPaneLogRetryScanIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.log")
	for _, input := range []string{"provider error", "clean retry", "provider error", "another clean retry"} {
		scan := testPaneLogScan(t, "provider error")
		require.NoError(t, DrainPaneLog(strings.NewReader(input), path, scan.path))
		pattern, err := scan.wait(t.Context())
		require.NoError(t, err)
		if strings.Contains(input, "provider error") {
			assert.Equal(t, new("provider error"), pattern)
		} else {
			assert.Nil(t, pattern)
		}
	}
}

func TestPaneLogScanPublishesOnlyAfterEOF(t *testing.T) {
	scan := testPaneLogScan(t, "first error", "second error")
	input, output := io.Pipe()
	t.Cleanup(func() { _ = input.Close(); _ = output.Close() })
	done := make(chan error, 1)
	go func() { done <- DrainPaneLog(input, "", scan.path) }()
	_, err := io.WriteString(output, "second error")
	require.NoError(t, err)
	_, err = os.Stat(scan.path + ".result")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = io.WriteString(output, "first error")
	require.NoError(t, err)
	require.NoError(t, output.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not finish at EOF")
	}
	pattern, err := scan.wait(t.Context())
	require.NoError(t, err)
	assert.Equal(t, new("first error"), pattern)
}

func TestPaneLogScanCoverageErrors(t *testing.T) {
	for _, kind := range []string{"missing", "incomplete", "scan error", "invalid JSON", "unconfigured pattern"} {
		t.Run(kind, func(t *testing.T) {
			scan := testPaneLogScan(t, "provider error")
			switch kind {
			case "incomplete":
				require.NoError(t, publishPaneLogScan(scan.path, paneLogScanResult{}))
			case "scan error":
				require.NoError(t, publishPaneLogScan(scan.path, paneLogScanResult{Complete: true, Error: "read failed"}))
			case "invalid JSON":
				require.NoError(t, os.WriteFile(scan.path+".result", []byte("{"), 0o600))
			case "unconfigured pattern":
				require.NoError(t, publishPaneLogScan(scan.path, paneLogScanResult{Complete: true, FailurePattern: new("another pattern")}))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			_, err := scan.wait(ctx)
			require.Error(t, err)
		})
	}
}

type failedPaneReader struct{}

func (failedPaneReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestPaneLogScanReadErrorFailsClosed(t *testing.T) {
	scan := testPaneLogScan(t, "provider error")
	input := io.MultiReader(strings.NewReader("provider error"), failedPaneReader{})
	require.ErrorIs(t, DrainPaneLog(input, "", scan.path), io.ErrUnexpectedEOF)
	_, err := scan.wait(t.Context())
	require.ErrorContains(t, err, "incomplete")
}

func TestPaneLogScanInvalidSpecStillDrains(t *testing.T) {
	for _, spec := range []string{"{", `{"patterns":["["]}`} {
		t.Run(spec, func(t *testing.T) {
			scan := testPaneLogScan(t, "provider error")
			require.NoError(t, os.WriteFile(scan.path, []byte(spec), 0o600))
			input := strings.NewReader("provider error")
			require.Error(t, DrainPaneLog(input, "", scan.path))
			assert.Zero(t, input.Len())
			_, err := scan.wait(t.Context())
			require.ErrorContains(t, err, "incomplete")
		})
	}
}

func TestDrainPaneLogRejectsExtraScanPaths(t *testing.T) {
	input := strings.NewReader("output")
	require.Error(t, DrainPaneLog(input, "", "first", "second"))
	assert.Zero(t, input.Len())
}

func TestPaneLogScanWaitsForCompletion(t *testing.T) {
	scan := testPaneLogScan(t, "provider error")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		pattern, err := scan.wait(ctx)
		if pattern == nil || *pattern != "provider error" {
			err = errors.Join(err, errors.New("missing pattern"))
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("wait returned before completion: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, publishPaneLogScan(scan.path, paneLogScanResult{Complete: true, FailurePattern: new("provider error")}))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not observe completion")
	}
}
