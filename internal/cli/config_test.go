package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/config"
)

func TestShowConfig(t *testing.T) {
	cfg := &config.Config{
		TicketsDir: "~/org/tickets",
		Web:        config.Web{Token: "web-token-value"},
		Notifications: config.Notifications{
			Channels: map[string]config.NotifyChannel{
				"tg": {Type: config.NotifyTelegram, SecretEnv: "KONTORA_TELEGRAM_TOKEN", ChatID: "42"},
				"hook": {
					Type:    config.NotifyWebhook,
					URL:     "https://hooks.example.com/services/T000/B111/xoxb-url-secret",
					Headers: map[string]string{"Authorization": "Bearer header-secret", "X-Team": "platform"},
				},
			},
			Default: []string{"tg"},
		},
	}

	var out strings.Builder
	require.NoError(t, ShowConfig(cfg, &out))
	got := out.String()

	assert.Contains(t, got, RedactedToken)
	assert.NotContains(t, got, "web-token-value")

	// A channel names where its credential lives; the value is resolved by the
	// daemon and never enters the struct this prints.
	assert.Contains(t, got, "KONTORA_TELEGRAM_TOKEN")
	assert.NotContains(t, got, "secret:", "the rejected-by-name field must not read as one to fill in")

	// A header is where a bearer token or an API key is written, and the path
	// of a webhook URL is the whole credential for a Slack-style hook. This
	// output is what people paste into issues.
	assert.NotContains(t, got, "header-secret")
	assert.NotContains(t, got, "xoxb-url-secret")
	assert.Contains(t, got, "https://hooks.example.com/", "the host says where it posts")
	assert.Contains(t, got, "X-Team", "a header name still shows, only its value is hidden")

	assert.Equal(t, "web-token-value", cfg.Web.Token, "printing must not mutate the caller's config")
	assert.Equal(t, "Bearer header-secret", cfg.Notifications.Channels["hook"].Headers["Authorization"],
		"printing must not mutate the caller's channels")
}
