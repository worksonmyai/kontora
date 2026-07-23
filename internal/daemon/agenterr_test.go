package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/worksonmyai/kontora/internal/config"
)

func TestScanClaudeSessionError(t *testing.T) {
	// A normal assistant turn followed by trailing bookkeeping lines.
	okSession := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"system","subtype":"init"}
`
	tests := []struct {
		name       string
		content    string
		wantReason string
		wantFound  bool
	}{
		{
			name:      "clean session",
			content:   okSession,
			wantFound: false,
		},
		{
			name:       "quota limit",
			content:    `{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit · resets 5pm (Europe/Amsterdam)"}]}}` + "\n",
			wantReason: "You've hit your limit · resets 5pm (Europe/Amsterdam)",
			wantFound:  true,
		},
		{
			name:       "api error not on last line",
			content:    `{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Repeated 529 Overloaded errors"}]}}` + "\n" + `{"type":"progress"}` + "\n" + `{"type":"last-prompt"}` + "\n",
			wantReason: "API Error: Repeated 529 Overloaded errors",
			wantFound:  true,
		},
		{
			name: "last error wins over earlier one",
			content: `{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"API Error: 500"}]}}` + "\n" +
				`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"Not logged in · Please run /login"}]}}` + "\n",
			wantReason: "Not logged in · Please run /login",
			wantFound:  true,
		},
		{
			name:       "flag set but content is a bare string",
			content:    `{"type":"assistant","isApiErrorMessage":true,"message":{"content":"Prompt is too long"}}` + "\n",
			wantReason: "Prompt is too long",
			wantFound:  true,
		},
		{
			name:       "flag set but no extractable text",
			content:    `{"type":"assistant","isApiErrorMessage":true,"message":{"content":[]}}` + "\n",
			wantReason: "agent reported an API error",
			wantFound:  true,
		},
		{
			name:      "malformed lines are skipped",
			content:   "not json\n{}\n" + `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n",
			wantFound: false,
		},
		{
			// A giant tool-result line must not abort the scan and hide a later
			// error entry (would be a false negative — the bug this guards).
			name: "error entry after an oversized line",
			content: `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + strings.Repeat("x", 2<<20) + `"}]}}` + "\n" +
				`{"type":"assistant","isApiErrorMessage":true,"message":{"content":[{"type":"text","text":"API Error: 500"}]}}` + "\n",
			wantReason: "API Error: 500",
			wantFound:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), 0o644))

			reason, found := scanClaudeSessionError(path)
			assert.Equal(t, tt.wantFound, found)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestScanClaudeSessionErrorMissingFile(t *testing.T) {
	reason, found := scanClaudeSessionError(filepath.Join(t.TempDir(), "nope.jsonl"))
	assert.False(t, found)
	assert.Empty(t, reason)
}

func TestDefaultFailurePatternsMatchRealErrors(t *testing.T) {
	positives := []string{
		"API Error: Repeated 529 Overloaded errors",
		`API Error: 500 {"type":"error"}`,
		"You've hit your limit · resets 5pm (Europe/Amsterdam)",
		"Claude AI usage limit reached",
		"Not logged in · Please run /login",
		"Invalid API key · Please run /login",
		"Prompt is too long",
		"openai: insufficient_quota: You exceeded your current quota",
		"Rate limit reached for gpt-4 in organization org-abc",
	}
	// Benign agent prose / source code that mentions the same domain words must
	// NOT trip detection — e.g. a ticket that implements rate limiting.
	negatives := []string{
		"I'll implement rate limiting for the querier module",
		"Added RateLimitIngestBytesBurst and a rate.Limiter",
		"the rate limit exceeded case returns HTTP 429",
		"func TestRateLimit(t *testing.T) {",
		"updated the quota accounting in overrides.go",
	}
	for _, s := range positives {
		_, ok := matchFailurePatterns(s, config.DefaultFailurePatterns)
		assert.Truef(t, ok, "expected default patterns to match %q", s)
	}
	for _, s := range negatives {
		_, ok := matchFailurePatterns(s, config.DefaultFailurePatterns)
		assert.Falsef(t, ok, "expected default patterns NOT to match %q", s)
	}
}

func TestMatchFailurePatterns(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		patterns  []string
		wantFound bool
	}{
		{
			name:      "no patterns",
			content:   "anything",
			patterns:  nil,
			wantFound: false,
		},
		{
			name:      "match",
			content:   "some log\nusage limit reached\nmore",
			patterns:  []string{"usage limit reached"},
			wantFound: true,
		},
		{
			name:      "no match",
			content:   "all good here",
			patterns:  []string{"usage limit reached", "^API Error:"},
			wantFound: false,
		},
		{
			name:      "second pattern matches",
			content:   "API Error: 500 internal",
			patterns:  []string{"quota exceeded", "^API Error:"},
			wantFound: true,
		},
		{
			name:      "invalid regex is skipped, valid one still matches",
			content:   "boom",
			patterns:  []string{"[unterminated", "boom"},
			wantFound: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, found := matchFailurePatterns(tt.content, tt.patterns)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.NotEmpty(t, reason)
			}
		})
	}
}
