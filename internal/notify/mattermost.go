package notify

import (
	"context"
	"encoding/json"
	"net/http"
)

// Mattermost posts to an incoming webhook. The webhook URL is the credential,
// so it comes from secret_env or secret_file rather than from the config file.
type Mattermost struct {
	ChannelName string
	URL         string
	// Channel overrides the channel the incoming webhook was created against.
	Channel string
}

func (m *Mattermost) Name() string { return m.ChannelName }

func (m *Mattermost) Request(ctx context.Context, e Event) (*http.Request, error) {
	if m.URL == "" {
		return nil, errNoCredential
	}
	payload := map[string]string{"text": render(e)}
	if m.Channel != "" {
		payload["channel"] = m.Channel
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return post(ctx, http.MethodPost, m.URL, body, nil)
}
