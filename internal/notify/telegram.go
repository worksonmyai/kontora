package notify

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"net/url"
)

// telegramAPI is the Bot API root a Telegram channel posts to unless BaseURL
// names another one.
const telegramAPI = "https://api.telegram.org"

// Telegram sends through the Bot API's sendMessage. The token is the whole
// credential and rides in the path, which is why it is never logged: a daemon
// log line carrying the URL would carry the token.
type Telegram struct {
	ChannelName string
	Token       string
	ChatID      string
	// BaseURL overrides the Bot API root, for a self-hosted Bot API server or a
	// test. Empty means telegramAPI.
	BaseURL string
}

func (t *Telegram) Name() string { return t.ChannelName }

func (t *Telegram) Request(ctx context.Context, e Event) (*http.Request, error) {
	if t.Token == "" {
		return nil, errNoCredential
	}
	base := t.BaseURL
	if base == "" {
		base = telegramAPI
	}
	body, err := json.Marshal(map[string]string{
		"chat_id": t.ChatID,
		// The rendered text carries ticket titles and summaries, which can hold
		// any character; HTML mode would read an unescaped one as markup.
		"text":       html.EscapeString(render(e)),
		"parse_mode": "HTML",
	})
	if err != nil {
		return nil, err
	}
	return post(ctx, http.MethodPost, base+"/bot"+url.PathEscape(t.Token)+"/sendMessage", body, nil)
}
