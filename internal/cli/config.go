package cli

import (
	"io"
	"net/url"

	"gopkg.in/yaml.v3"

	"github.com/worksonmyai/kontora/internal/config"
)

// RedactedToken is what ShowConfig prints in place of a credential.
const RedactedToken = "<redacted>"

// ShowConfig writes the effective configuration (with defaults applied) as
// YAML, with every value that can be a credential replaced by a placeholder:
// this output is what people paste into issues and chat threads. `kontora
// config edit` shows the real file when a value itself needs checking.
func ShowConfig(cfg *config.Config, w io.Writer) error {
	redacted := *cfg
	if redacted.Web.Token != "" {
		redacted.Web.Token = RedactedToken
	}
	redacted.Notifications.Channels = redactNotifyChannels(cfg.Notifications.Channels)
	cfg = &redacted

	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(cfg); err != nil {
		return err
	}
	return enc.Close()
}

// redactNotifyChannels hides the two parts of a channel that can hold a secret
// even though the channel's own credential is only ever named, not written: a
// header, where a bearer token or an API key goes, and the path of a webhook
// URL, which for a Slack-style incoming webhook is the whole credential. The
// scheme and the host survive, so the row still says where it posts.
func redactNotifyChannels(channels map[string]config.NotifyChannel) map[string]config.NotifyChannel {
	if len(channels) == 0 {
		return channels
	}
	out := make(map[string]config.NotifyChannel, len(channels))
	for name, ch := range channels {
		if len(ch.Headers) > 0 {
			hdr := make(map[string]string, len(ch.Headers))
			for k := range ch.Headers {
				hdr[k] = RedactedToken
			}
			ch.Headers = hdr
		}
		ch.URL = redactURLPath(ch.URL)
		out[name] = ch
	}
	return out
}

// redactURLPath keeps the scheme and the host of raw and replaces the rest.
func redactURLPath(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return RedactedToken
	}
	return u.Scheme + "://" + u.Host + "/" + RedactedToken
}
