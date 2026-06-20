// Package remote provides an HTTP client for driving a kontora daemon over its
// web API. It is the single client used by both the local TUI (talking to a
// daemon on localhost) and the CLI's remote mode (talking to a daemon over a
// tailnet). A non-empty token is sent as an Authorization: Bearer header on
// every request.
package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/web"
)

// Client talks to a kontora daemon's HTTP/SSE API.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New returns a Client for the given base URL (e.g. http://host:8080) and
// bearer token. An empty token disables auth headers.
func New(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: 10 * time.Second},
	}
}

// NewWithClient is New but with a caller-supplied http.Client, used by tests
// and callers that need custom transport or timeouts.
func NewWithClient(base, token string, hc *http.Client) *Client {
	c := New(base, token)
	if hc != nil {
		c.hc = hc
	}
	return c
}

// Base returns the daemon base URL.
func (c *Client) Base() string { return c.base }

// Token returns the configured bearer token.
func (c *Client) Token() string { return c.token }

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// doJSON performs a request with an optional JSON body and decodes a JSON
// response into out (when non-nil). Non-2xx responses become errors.
func (c *Client) doJSON(method, path string, reqBody, out any) error {
	var rdr io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := c.newRequest(context.Background(), method, path, rdr)
	if err != nil {
		return err
	}
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	var r struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if r.Error != "" {
		return fmt.Errorf("%s", r.Error)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

type listResponse struct {
	Tickets       []web.TicketInfo `json:"tickets"`
	RunningAgents int              `json:"running_agents"`
}

// Ping checks that the daemon is reachable and authenticated.
func (c *Client) Ping(ctx context.Context) error {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/tickets", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ListTickets returns all board tickets and the running-agent count.
func (c *Client) ListTickets() ([]web.TicketInfo, int, error) {
	var r listResponse
	if err := c.doJSON(http.MethodGet, "/api/tickets", nil, &r); err != nil {
		return nil, 0, err
	}
	return r.Tickets, r.RunningAgents, nil
}

// GetTicket fetches a single ticket by exact ID.
func (c *Client) GetTicket(id string) (web.TicketInfo, error) {
	req, err := c.newRequest(context.Background(), http.MethodGet, "/api/tickets/"+id, nil)
	if err != nil {
		return web.TicketInfo{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return web.TicketInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return web.TicketInfo{}, fmt.Errorf("ticket %q not found", id)
	}
	if resp.StatusCode >= 400 {
		return web.TicketInfo{}, decodeError(resp)
	}
	var info web.TicketInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return web.TicketInfo{}, err
	}
	return info, nil
}

// Logs fetches log output for a ticket stage. An empty stage returns the most
// recent log.
func (c *Client) Logs(id, stage string) (string, error) {
	path := "/api/tickets/" + id + "/logs"
	if stage != "" {
		path += "?stage=" + url.QueryEscape(stage)
	}
	req, err := c.newRequest(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("logs not found")
	}
	if resp.StatusCode >= 400 {
		return "", decodeError(resp)
	}
	var r struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.Content, nil
}

// Config returns the daemon's pipelines, agents, and related metadata.
func (c *Client) Config() (web.ConfigInfo, error) {
	var cfg web.ConfigInfo
	if err := c.doJSON(http.MethodGet, "/api/config", nil, &cfg); err != nil {
		return web.ConfigInfo{}, err
	}
	return cfg, nil
}

// CreateTicket creates a ticket on the daemon host.
func (c *Client) CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error) {
	var info web.TicketInfo
	if err := c.doJSON(http.MethodPost, "/api/tickets", req, &info); err != nil {
		return web.TicketInfo{}, err
	}
	return info, nil
}

// UpdateTicket updates body/frontmatter fields of a ticket.
func (c *Client) UpdateTicket(id string, req web.UpdateTicketRequest) error {
	return c.doJSON(http.MethodPut, "/api/tickets/"+id, req, nil)
}

func (c *Client) postAction(path string) error {
	return c.doJSON(http.MethodPost, path, nil, nil)
}

// Pause pauses a running or queued ticket.
func (c *Client) Pause(id string) error { return c.postAction("/api/tickets/" + id + "/pause") }

// Retry re-queues a non-running ticket.
func (c *Client) Retry(id string) error { return c.postAction("/api/tickets/" + id + "/retry") }

// Run enqueues an open or todo ticket for processing.
func (c *Client) Run(id string) error { return c.postAction("/api/tickets/" + id + "/run") }

// Skip advances a ticket to the next pipeline stage.
func (c *Client) Skip(id string) error { return c.postAction("/api/tickets/" + id + "/skip") }

// SetStage moves a ticket to a specific pipeline stage by name.
func (c *Client) SetStage(id, stage string) error {
	return c.doJSON(http.MethodPost, "/api/tickets/"+id+"/set-stage", map[string]string{"stage": stage}, nil)
}

// Move sets a ticket's status via the move endpoint.
func (c *Client) Move(id, status string) error {
	return c.doJSON(http.MethodPost, "/api/tickets/"+id+"/move", map[string]string{"status": status}, nil)
}

// Cancel marks a ticket cancelled.
func (c *Client) Cancel(id string) error { return c.Move(id, "cancelled") }

// Done marks a ticket done.
func (c *Client) Done(id string) error { return c.Move(id, "done") }

// Note appends a timestamped note to a ticket.
func (c *Client) Note(id, text string) error {
	return c.doJSON(http.MethodPost, "/api/tickets/"+id+"/note", map[string]string{"text": text}, nil)
}

// ResolveID expands a ticket ID prefix to a full ID by listing tickets and
// matching client-side, mirroring DiskRepo.Resolve: an exact match wins,
// otherwise the first prefix match is returned.
func (c *Client) ResolveID(input string) (string, error) {
	tickets, _, err := c.ListTickets()
	if err != nil {
		return "", err
	}
	var prefix string
	for _, t := range tickets {
		if t.ID == input {
			return input, nil
		}
		if prefix == "" && strings.HasPrefix(t.ID, input) {
			prefix = t.ID
		}
	}
	if prefix != "" {
		return prefix, nil
	}
	return "", fmt.Errorf("ticket %q not found", input)
}

// Subscribe streams ticket events over SSE until ctx is cancelled.
func (c *Client) Subscribe(ctx context.Context) <-chan web.TicketEvent {
	ch := make(chan web.TicketEvent, 64)
	go func() {
		defer close(ch)
		c.sseLoop(ctx, ch)
	}()
	return ch
}

func (c *Client) sseLoop(ctx context.Context, ch chan<- web.TicketEvent) {
	for {
		if ctx.Err() != nil {
			return
		}
		req, err := c.newRequest(ctx, http.MethodGet, "/api/events", nil)
		if err != nil {
			return
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			// A bad token never recovers, so reconnecting is pointless. Any other
			// non-200 (proxy 5xx, daemon mid-restart) gets the same backoff as a
			// transport error rather than a tight reconnect loop.
			if resp.StatusCode == http.StatusUnauthorized {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
				continue
			}
		}

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		var eventType string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data := strings.TrimPrefix(line, "data: ")
				var info web.TicketInfo
				if json.Unmarshal([]byte(data), &info) == nil {
					ev := web.TicketEvent{Type: eventType, Ticket: info}
					select {
					case ch <- ev:
					default:
					}
				}
				eventType = ""
			}
		}
		resp.Body.Close()
	}
}
