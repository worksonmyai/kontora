package tui

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/remote"
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
	client := remote.New(base, cfg.Web.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err == nil {
		return &apiSource{client: client}
	}
	return &fileSource{cfg: cfg}
}

// --- apiSource ---

// apiSource adapts a remote.Client to the TUI Source interface.
type apiSource struct {
	client *remote.Client
}

func (s *apiSource) Connected() bool { return true }

func (s *apiSource) FetchTickets() ([]web.TicketInfo, int, error) {
	return s.client.ListTickets()
}

func (s *apiSource) FetchTask(id string) (web.TicketInfo, error) {
	return s.client.GetTicket(id)
}

func (s *apiSource) FetchLogs(id, stage string) (string, error) {
	return s.client.Logs(id, stage)
}

func (s *apiSource) FetchConfig() (web.ConfigInfo, error) {
	return s.client.Config()
}

func (s *apiSource) CreateTicket(req web.CreateTicketRequest) (web.TicketInfo, error) {
	return s.client.CreateTicket(req)
}

func (s *apiSource) UpdateTicket(id string, req web.UpdateTicketRequest) error {
	return s.client.UpdateTicket(id, req)
}

func (s *apiSource) PauseTicket(id string) error { return s.client.Pause(id) }
func (s *apiSource) RetryTicket(id string) error { return s.client.Retry(id) }
func (s *apiSource) SkipStage(id string) error   { return s.client.Skip(id) }

func (s *apiSource) SetStage(id, stage string) error {
	return s.client.SetStage(id, stage)
}

func (s *apiSource) Subscribe(ctx context.Context) <-chan web.TicketEvent {
	return s.client.Subscribe(ctx)
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
		if info.Kontora && info.Status == string(ticket.StatusInProgress) {
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
