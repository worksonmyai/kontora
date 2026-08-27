package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// bodyPrefix is how much of a rejected response is logged. A token error says
// what is wrong in its first line, and the rest is noise in a daemon log.
const bodyPrefix = 200

// maxDrain caps how much of an accepted response is read before the body is
// closed.
const maxDrain = 1 << 20

// post builds one JSON request. Channels call it per attempt: an http.Request
// carries its body as a reader, so reusing one across attempts would send an
// empty body the second time.
func post(ctx context.Context, method, url string, body []byte, hdr map[string]string) (*http.Request, error) {
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// In name order: Header.Set canonicalizes the name, so two keys differing
	// only in case land on one header and map order would decide which value
	// went out.
	for _, k := range slices.Sorted(maps.Keys(hdr)) {
		req.Header.Set(k, hdr[k])
	}
	return req, nil
}

// deliver sends one event to one channel, retrying a transport error, a 429 or
// a 5xx up to Attempts times with doubling backoff. Any other 4xx is a
// configuration mistake, a wrong token or a deleted chat, and retrying it would
// only repeat the same rejection, so it is logged with the status and the start
// of the body, which is what tells the user what to fix.
func (d *Dispatcher) deliver(ctx context.Context, ch Channel, e Event) {
	backoff := d.backoff
	for attempt := 1; ; attempt++ {
		retry, err := d.attempt(ctx, ch, e)
		if err == nil {
			d.log.Info("notification sent",
				"channel", ch.Name(), "ticket", e.TicketID, "status", e.To, "attempts", attempt)
			d.result(ch.Name(), ResultOK)
			return
		}
		if !retry || attempt >= d.attempts {
			d.log.Warn("notification failed, dropping",
				"channel", ch.Name(), "ticket", e.TicketID, "status", e.To,
				"attempts", attempt, "err", err)
			d.result(ch.Name(), ResultFailed)
			return
		}
		select {
		case <-ctx.Done():
			d.result(ch.Name(), ResultFailed)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// attempt makes one request and reports whether its failure is worth retrying.
func (d *Dispatcher) attempt(ctx context.Context, ch Channel, e Event) (retry bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	req, err := ch.Request(reqCtx, e)
	if err != nil {
		return false, fmt.Errorf("building request: %w", redactURL(err))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		// A cancelled daemon is not a transport failure worth retrying.
		if ctx.Err() != nil {
			return false, redactURL(err)
		}
		return true, redactURL(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drained so the connection can be reused, capped so a runaway body
		// cannot be read forever.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
		return false, nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, bodyPrefix))
	err = fmt.Errorf("http %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, err
}

// redactURL strips the request URL out of an error before it can be logged.
// http.Client and url.Parse both fail with a *url.Error, whose Error() embeds
// the URL, and that URL is the credential: the bot token is in the path for
// telegram, and for mattermost the whole incoming-webhook URL is the secret.
func redactURL(err error) error {
	if uerr, ok := errors.AsType[*url.Error](err); ok {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}

// errNoCredential is what a channel returns when the daemon handed it an empty
// secret. It cannot happen through the config path, which drops such a channel
// before the dispatcher sees it.
var errNoCredential = errors.New("channel has no credential")
