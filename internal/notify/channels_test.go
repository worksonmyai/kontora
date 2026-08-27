package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testEvent() Event {
	return Event{
		TicketID: "kon-a",
		From:     "in_progress",
		To:       "done",
		At:       time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		Fields: Fields{
			Title:   "Add <notify> channels",
			Stage:   "implement",
			Branch:  "kontora/notify",
			Project: "kontora",
			Summary: "wired the dispatcher",
		},
	}
}

func readBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	return body
}

func TestRender(t *testing.T) {
	tests := []struct {
		name     string
		event    Event
		contains []string
		absent   []string
	}{
		{
			name:     "a status change names both statuses",
			event:    testEvent(),
			contains: []string{"kon-a: done (was in_progress)", "Add <notify> channels", "project: kontora | stage: implement | branch: kontora/notify", "wired the dispatcher"},
		},
		{
			name:     "a first-seen from is left out",
			event:    Event{TicketID: "kon-a", To: "done"},
			contains: []string{"kon-a: done"},
			absent:   []string{"was"},
		},
		{
			name:     "a waiting event carries the question",
			event:    Event{TicketID: "kon-a", To: StatusWaiting, Fields: Fields{Question: "which one?"}},
			contains: []string{"kon-a: waiting", "which one?"},
		},
		{
			name:     "a pause carries the error",
			event:    Event{TicketID: "kon-a", From: "in_progress", To: "paused", Fields: Fields{LastError: "runner failed"}},
			contains: []string{"runner failed"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(tt.event)
			for _, want := range tt.contains {
				assert.Contains(t, got, want)
			}
			for _, unwanted := range tt.absent {
				assert.NotContains(t, got, unwanted)
			}
		})
	}
}

func TestRenderTruncatesAnOverlongMessage(t *testing.T) {
	tests := []struct {
		name string
		fill string
		// units is how many UTF-16 code units one fill costs. Telegram counts
		// those, not runes, so an emoji summary that looked half the limit long
		// used to go out over it and be rejected whole.
		units int
	}{
		{name: "ascii", fill: "x", units: 1},
		{name: "emoji", fill: "\U0001F600", units: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := testEvent()
			e.Fields.Summary = strings.Repeat(tt.fill, maxMessage)
			got := render(e)
			require.True(t, strings.HasSuffix(got, "(truncated)"))
			assert.LessOrEqual(t, len(utf16.Encode([]rune(got))), maxMessage+len("\n(truncated)"))
		})
	}
}

func TestTelegramRequest(t *testing.T) {
	tg := &Telegram{ChannelName: "tg", Token: "123:secret", ChatID: "42", BaseURL: "https://example.invalid"}
	req, err := tg.Request(t.Context(), testEvent())
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, req.Method)
	assert.Equal(t, "/bot123:secret/sendMessage", req.URL.Path)
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))

	body := readBody(t, req)
	assert.Equal(t, "42", body["chat_id"])
	assert.Equal(t, "HTML", body["parse_mode"])
	text, _ := body["text"].(string)
	assert.Contains(t, text, "&lt;notify&gt;", "HTML mode would read an unescaped title as markup")
	assert.NotContains(t, text, "<notify>")
}

func TestTelegramNeedsAToken(t *testing.T) {
	tg := &Telegram{ChannelName: "tg", ChatID: "42"}
	_, err := tg.Request(t.Context(), testEvent())
	assert.ErrorIs(t, err, errNoCredential)
}

func TestMattermostRequest(t *testing.T) {
	tests := []struct {
		name        string
		channel     string
		wantChannel any
	}{
		{name: "the webhook's own channel", wantChannel: nil},
		{name: "an override", channel: "town-square", wantChannel: "town-square"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mm := &Mattermost{ChannelName: "mm", URL: "https://example.invalid/hooks/abc", Channel: tt.channel}
			req, err := mm.Request(t.Context(), testEvent())
			require.NoError(t, err)

			assert.Equal(t, http.MethodPost, req.Method)
			assert.Equal(t, "/hooks/abc", req.URL.Path)

			body := readBody(t, req)
			assert.Equal(t, tt.wantChannel, body["channel"])
			text, _ := body["text"].(string)
			assert.Contains(t, text, "<notify>", "only telegram escapes for HTML")
		})
	}
}

func TestWebhookRequest(t *testing.T) {
	w := &Webhook{
		ChannelName: "hook",
		URL:         "https://example.invalid/ingest",
		Method:      http.MethodPut,
		Headers:     map[string]string{"X-Team": "kontora"},
		Token:       "shhh",
	}
	req, err := w.Request(t.Context(), testEvent())
	require.NoError(t, err)

	assert.Equal(t, http.MethodPut, req.Method)
	assert.Equal(t, "kontora", req.Header.Get("X-Team"))
	assert.Equal(t, "Bearer shhh", req.Header.Get("Authorization"))
	assert.Empty(t, w.Headers["Authorization"], "the bearer header must not be written back into the configured map")

	body := readBody(t, req)
	assert.Equal(t, "kon-a", body["ticket"])
	assert.Equal(t, "in_progress", body["from"])
	assert.Equal(t, "done", body["to"])
	assert.Equal(t, "implement", body["stage"])
	assert.Contains(t, body["text"], "kon-a: done")
}

func TestWebhookDefaultsToPost(t *testing.T) {
	w := &Webhook{ChannelName: "hook", URL: "https://example.invalid/ingest"}
	req, err := w.Request(t.Context(), testEvent())
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, req.Method)
	assert.Empty(t, req.Header.Get("Authorization"))
}

// TestWebhookHeaderCaseIsDecidedNotRaced covers a header written in the config
// whose name canonicalizes onto one the channel sets itself. net/http folds the
// two into one header, and map order used to decide which value went out.
func TestWebhookHeaderCaseIsDecidedNotRaced(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		token   string
		header  string
		want    string
	}{
		{
			name:    "the resolved token wins over a configured authorization",
			headers: map[string]string{"authorization": "Bearer from-config"},
			token:   "from-secret",
			header:  "Authorization",
			want:    "Bearer from-secret",
		},
		{
			name:    "a lowercase header still reaches its canonical name",
			headers: map[string]string{"x-api-key": "k"},
			token:   "t",
			header:  "X-Api-Key",
			want:    "k",
		},
		{
			name:    "a configured content-type overrides the default",
			headers: map[string]string{"content-type": "application/vnd.kontora+json"},
			header:  "Content-Type",
			want:    "application/vnd.kontora+json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &Webhook{ChannelName: "hook", URL: "https://example.invalid/ingest", Headers: tt.headers, Token: tt.token}
			// Repeated because the failure it guards is a randomized map order,
			// which one pass has a good chance of getting right by luck.
			for range 50 {
				req, err := w.Request(t.Context(), testEvent())
				require.NoError(t, err)
				require.Equal(t, tt.want, req.Header.Get(tt.header))
			}
		})
	}
}
