package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

// Source abstracts the data backend for the TUI. There are two
// implementations: apiSource (daemon running) and fileSource (daemon not
// running, reads ticket files directly from disk).
type Source interface {
	FetchTickets() ([]web.TicketInfo, int, error)
	FetchTask(id string) (web.TicketInfo, error)
	FetchLogs(id, stage string) (string, error)
	FetchConfig() (web.ConfigInfo, error)
	CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error)
	UpdateTicket(id string, req web.UpdateTicketRequest) error
	PauseTicket(id string) error
	RetryTicket(id string) error
	SkipStage(id string) error
	SetStage(id, stage string) error
	Subscribe(ctx context.Context) <-chan web.TicketEvent
	Connected() bool
}

// newSource probes the daemon API; if reachable, returns an apiSource,
// otherwise falls back to fileSource.
func newSource(cfg *config.Config) Source {
	if cfg.Web.Enabled != nil && !*cfg.Web.Enabled {
		return &fileSource{cfg: cfg}
	}
	base := "http://" + net.JoinHostPort(cfg.Web.Host, fmt.Sprintf("%d", cfg.Web.Port))
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/api/tickets")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return &apiSource{base: base, client: &http.Client{Timeout: 10 * time.Second}}
		}
	}
	return &fileSource{cfg: cfg}
}

// --- apiSource ---

type apiSource struct {
	base   string
	client *http.Client
}

func (s *apiSource) Connected() bool { return true }

type ticketsResponse struct {
	Tickets       []web.TicketInfo `json:"tickets"`
	RunningAgents int              `json:"running_agents"`
}

func (s *apiSource) FetchTickets() ([]web.TicketInfo, int, error) {
	resp, err := s.client.Get(s.base + "/api/tickets")
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d fetching tickets", resp.StatusCode)
	}
	var r ticketsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, 0, err
	}
	return r.Tickets, r.RunningAgents, nil
}

func (s *apiSource) FetchTask(id string) (web.TicketInfo, error) {
	resp, err := s.client.Get(s.base + "/api/tickets/" + id)
	if err != nil {
		return web.TicketInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return web.TicketInfo{}, fmt.Errorf("ticket %q not found", id)
	}
	if resp.StatusCode != http.StatusOK {
		return web.TicketInfo{}, fmt.Errorf("HTTP %d fetching ticket %q", resp.StatusCode, id)
	}
	var info web.TicketInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return web.TicketInfo{}, err
	}
	return info, nil
}

func (s *apiSource) FetchLogs(id, stage string) (string, error) {
	u := s.base + "/api/tickets/" + id + "/logs"
	if stage != "" {
		u += "?stage=" + url.QueryEscape(stage)
	}
	resp, err := s.client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("logs not found")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching logs", resp.StatusCode)
	}
	var r struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.Content, nil
}

func (s *apiSource) postAction(path string) error {
	resp, err := s.client.Post(s.base+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var r struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&r)
		if r.Error != "" {
			return fmt.Errorf("%s", r.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *apiSource) FetchConfig() (web.ConfigInfo, error) {
	resp, err := s.client.Get(s.base + "/api/config")
	if err != nil {
		return web.ConfigInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return web.ConfigInfo{}, fmt.Errorf("HTTP %d fetching config", resp.StatusCode)
	}
	var cfg web.ConfigInfo
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return web.ConfigInfo{}, err
	}
	return cfg, nil
}

func (s *apiSource) CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return web.TicketInfo{}, err
	}
	resp, err := s.client.Post(s.base+"/api/tickets", "application/json", bytes.NewReader(body))
	if err != nil {
		return web.TicketInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var r struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&r)
		if r.Error != "" {
			return web.TicketInfo{}, fmt.Errorf("%s", r.Error)
		}
		return web.TicketInfo{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var info web.TicketInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return web.TicketInfo{}, err
	}
	return info, nil
}

func (s *apiSource) UpdateTicket(id string, req web.UpdateTicketRequest) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequest(http.MethodPut, s.base+"/api/tickets/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var r struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&r)
		if r.Error != "" {
			return fmt.Errorf("%s", r.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *apiSource) PauseTicket(id string) error {
	return s.postAction("/api/tickets/" + id + "/pause")
}
func (s *apiSource) RetryTicket(id string) error {
	return s.postAction("/api/tickets/" + id + "/retry")
}
func (s *apiSource) SkipStage(id string) error { return s.postAction("/api/tickets/" + id + "/skip") }

func (s *apiSource) SetStage(id, stage string) error {
	body, err := json.Marshal(struct {
		Stage string `json:"stage"`
	}{stage})
	if err != nil {
		return err
	}
	resp, err := s.client.Post(s.base+"/api/tickets/"+id+"/set-stage", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var r struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&r)
		if r.Error != "" {
			return fmt.Errorf("%s", r.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *apiSource) Subscribe(ctx context.Context) <-chan web.TicketEvent {
	ch := make(chan web.TicketEvent, 64)
	go func() {
		defer close(ch)
		s.sseLoop(ctx, ch)
	}()
	return ch
}

func (s *apiSource) sseLoop(ctx context.Context, ch chan<- web.TicketEvent) {
	for {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/api/events", nil)
		if err != nil {
			return
		}
		resp, err := s.client.Do(req)
		if err != nil {
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

// --- fileSource ---

type fileSource struct {
	cfg *config.Config
}

func (s *fileSource) Connected() bool { return false }

func (s *fileSource) FetchTickets() ([]web.TicketInfo, int, error) {
	svc := s.newService()
	views, err := svc.List(app.ListOptions{})
	if err != nil {
		return nil, 0, err
	}

	var tickets []web.TicketInfo
	running := 0
	for _, v := range views {
		s.augmentStages(&v)
		info := web.TicketInfoFromView(v)
		if info.Status == string(ticket.StatusInProgress) {
			running++
		}
		tickets = append(tickets, info)
	}
	return tickets, running, nil
}

func (s *fileSource) FetchTask(id string) (web.TicketInfo, error) {
	svc := s.newService()
	v, err := svc.Get(id, app.GetOptions{IncludeBody: true})
	if err != nil {
		return web.TicketInfo{}, fmt.Errorf("ticket %q not found", id)
	}
	s.augmentStages(&v)
	return web.TicketInfoFromView(v), nil
}

func (s *fileSource) FetchLogs(id, stage string) (string, error) {
	var buf bytes.Buffer
	if err := cli.Logs(s.cfg.TicketsDir, s.cfg.LogsDir, id, stage, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

var errDaemonNotRunning = fmt.Errorf("daemon not running — actions unavailable in file mode")

func (s *fileSource) FetchConfig() (web.ConfigInfo, error) {
	return web.ConfigInfo{}, errDaemonNotRunning
}
func (s *fileSource) CreateTicket(web.CreateTicketRequest) (web.TicketInfo, error) {
	return web.TicketInfo{}, errDaemonNotRunning
}
func (s *fileSource) UpdateTicket(string, web.UpdateTicketRequest) error { return errDaemonNotRunning }
func (s *fileSource) PauseTicket(string) error                           { return errDaemonNotRunning }
func (s *fileSource) RetryTicket(string) error                           { return errDaemonNotRunning }
func (s *fileSource) SkipStage(string) error                             { return errDaemonNotRunning }
func (s *fileSource) SetStage(string, string) error                      { return errDaemonNotRunning }
func (s *fileSource) Subscribe(context.Context) <-chan web.TicketEvent   { return nil }

func (s *fileSource) newService() *app.Service {
	return app.New(s.cfg, store.NewDiskRepo(s.cfg.TicketsDir), app.NoopRuntime{})
}

// augmentStages discovers log-file stages for simple kontora tickets.
func (s *fileSource) augmentStages(v *app.View) {
	if !v.Kontora || v.Pipeline != "" || len(v.Stages) == 0 {
		return
	}
	if v.Stages[0] != "default" {
		return
	}
	logsDir := config.ExpandTilde(s.cfg.LogsDir)
	logDir := filepath.Join(logsDir, v.ID)
	if entries, err := os.ReadDir(logDir); err == nil {
		var discovered []string
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".log") {
				discovered = append(discovered, strings.TrimSuffix(entry.Name(), ".log"))
			}
		}
		if len(discovered) > 0 {
			v.Stages = discovered
		}
	}
}

