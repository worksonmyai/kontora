package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Webhook posts the event as JSON to an endpoint of the user's choosing. Unlike
// the two chat channels it sends the fields rather than the rendered text, so
// whatever is on the other end can format its own message; the rendered text
// rides along as "text" so a receiver that only forwards has something to send.
type Webhook struct {
	ChannelName string
	URL         string
	Method      string
	Headers     map[string]string
	// Token, when set, is sent as a bearer credential. It comes from
	// secret_env or secret_file and is optional: a webhook may need none. It
	// wins over an Authorization header written in Headers, which is otherwise
	// resolved by map order.
	Token string
}

func (w *Webhook) Name() string { return w.ChannelName }

// webhookPayload is the wire form of an Event. It is declared rather than
// marshalling Event directly so a field rename inside the package cannot
// silently change what receivers parse.
type webhookPayload struct {
	Ticket    string    `json:"ticket"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to"`
	At        time.Time `json:"at"`
	Title     string    `json:"title,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	RepoPath  string    `json:"repo_path,omitempty"`
	Project   string    `json:"project,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	LastError string    `json:"last_error,omitempty"`
	Question  string    `json:"question,omitempty"`
	Text      string    `json:"text"`
}

func (w *Webhook) Request(ctx context.Context, e Event) (*http.Request, error) {
	f := e.Fields
	body, err := json.Marshal(webhookPayload{
		Ticket:    e.TicketID,
		From:      e.From,
		To:        e.To,
		At:        e.At,
		Title:     f.Title,
		Stage:     f.Stage,
		Branch:    f.Branch,
		RepoPath:  f.RepoPath,
		Project:   f.Project,
		Summary:   f.Summary,
		LastError: f.LastError,
		Question:  f.Question,
		Text:      render(e),
	})
	if err != nil {
		return nil, err
	}
	hdr := w.Headers
	if w.Token != "" {
		// Canonicalized on the way in so a configured "authorization" and the
		// injected one are one key rather than two that collide inside
		// Header.Set, where map order would pick the winner.
		hdr = make(map[string]string, len(w.Headers)+1)
		for k, v := range w.Headers {
			hdr[http.CanonicalHeaderKey(k)] = v
		}
		hdr["Authorization"] = "Bearer " + w.Token
	}
	return post(ctx, w.Method, w.URL, body, hdr)
}
