package tui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/worksonmyai/kontora/internal/cli"
	"github.com/worksonmyai/kontora/internal/cli/remote"
	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/logfmt"
	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/ticket/app"
	"github.com/worksonmyai/kontora/internal/ticket/store"
	"github.com/worksonmyai/kontora/internal/web"
)

// Source abstracts the data backend for the TUI. There are two
// implementations: apiSource (daemon verified for the requested directory)
// and fileSource (daemon unverified, reads ticket files directly from disk).
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
	Diagnostic() string
	Attachment() (session, socket string)
}

func newSource(cfg *config.Config) Source {
	if dir, err := config.DirectoryIdentity(cfg.TicketsDir); err == nil {
		local := *cfg
		local.TicketsDir = dir
		cfg = &local
	}
	fallback := &fileSource{cfg: cfg, diagnostic: "daemon unavailable; actions and attachment disabled"}
	if cfg.Web.Enabled != nil && !*cfg.Web.Enabled {
		fallback.diagnostic = "web API disabled; actions and attachment disabled"
		return fallback
	}
	client, err := remote.NewLocal(cfg.Web.Host, cfg.Web.Port, cfg.Web.Token)
	if err != nil {
		fallback.diagnostic = err.Error() + "; actions and attachment disabled"
		return fallback
	}
	client = client.GuardMutations(func(run func() error) error {
		return ticket.WithOrdinaryRouting(config.ExpandTilde(cfg.TicketsDir), run)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	identity, err := client.ConfigContext(ctx)
	if err != nil {
		fallback.diagnostic = "cannot verify daemon: " + err.Error() + "; actions and attachment disabled"
		return fallback
	}
	if !filepath.IsAbs(identity.TicketsDir) {
		fallback.diagnostic = "daemon directory identity missing; actions and attachment disabled"
		return fallback
	}
	matches, err := config.SameDirectory(cfg.TicketsDir, identity.TicketsDir)
	if err != nil {
		fallback.diagnostic = fmt.Sprintf("cannot compare daemon board %q with requested board %q: %v; actions and attachment disabled", identity.TicketsDir, cfg.TicketsDir, err)
		return fallback
	}
	if !matches {
		fallback.diagnostic = fmt.Sprintf("daemon board %q does not match requested board %q; actions and attachment disabled", identity.TicketsDir, cfg.TicketsDir)
		return fallback
	}
	return &apiSource{client: client, identity: identity}
}

// --- apiSource ---

// apiSource adapts a remote.Client to the TUI Source interface.
type apiSource struct {
	client    *remote.Client
	mu        sync.RWMutex
	configSeq uint64
	identity  web.ConfigInfo
}

func (s *apiSource) Connected() bool { return true }

func (s *apiSource) Attachment() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.identity.TmuxSession == "" || !filepath.IsAbs(s.identity.TmuxSocket) {
		return "", ""
	}
	return s.identity.TmuxSession, s.identity.TmuxSocket
}

func (s *apiSource) Diagnostic() string {
	if session, _ := s.Attachment(); session == "" {
		return "daemon socket identity unavailable; attachment disabled"
	}
	return ""
}

func (s *apiSource) FetchTickets() ([]web.TicketInfo, int, error) {
	return s.client.ListTickets()
}

func (s *apiSource) FetchTask(id string) (web.TicketInfo, error) {
	return s.client.GetTicket(id)
}

func (s *apiSource) FetchLogs(id, stage string) (string, error) {
	return s.client.Logs(id, stage)
}

func (s *apiSource) FetchRunLogs(id, stage string, run int) (string, error) {
	info, err := s.client.Activity(id, stage, run)
	if err != nil {
		return "", err
	}
	if info.Stale {
		return "No exact transcript for this attempt; the visit log was overwritten by a later retry.", nil
	}
	if info.Tape != nil {
		return renderRunTape(info.Tape), nil
	}
	return info.Content, nil
}

func (s *apiSource) RestartWorkflow(id string) error {
	return s.client.WorkflowRestart(id)
}

func (s *apiSource) FetchConfig() (web.ConfigInfo, error) {
	s.mu.Lock()
	s.configSeq++
	seq := s.configSeq
	s.mu.Unlock()
	cfg, err := s.client.Config()
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq != s.configSeq {
		return cfg, err
	}
	s.identity.TmuxSession, s.identity.TmuxSocket = "", ""
	if err != nil {
		return cfg, err
	}
	if !filepath.IsAbs(cfg.TicketsDir) {
		return cfg, fmt.Errorf("daemon directory identity missing")
	}
	matches, err := config.SameDirectory(s.identity.TicketsDir, cfg.TicketsDir)
	if err != nil {
		return cfg, fmt.Errorf("cannot compare daemon board identity: %w", err)
	}
	if !matches {
		return cfg, fmt.Errorf("daemon board identity changed")
	}
	s.identity.TmuxSession, s.identity.TmuxSocket = cfg.TmuxSession, cfg.TmuxSocket
	return cfg, nil
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
	cfg        *config.Config
	diagnostic string
}

func (s *fileSource) Connected() bool              { return false }
func (s *fileSource) Diagnostic() string           { return s.diagnostic }
func (s *fileSource) Attachment() (string, string) { return "", "" }

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
		if info.Kontora && info.Source == "" && info.IsRunning() {
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

func (s *fileSource) FetchRunLogs(id, stage string, run int) (string, error) {
	if !ticket.IsSafeID(id) || !ticket.IsSafeID(stage) || run < 0 {
		return "", fmt.Errorf("invalid run identity")
	}
	tk, err := ticket.ParseFile(filepath.Join(config.ExpandTilde(s.cfg.TicketsDir), id+".md"))
	if err != nil {
		return "", err
	}
	tape, content, err := cli.StageActivity(s.cfg.TicketsDir, s.cfg.LogsDir, id, stage, run)
	if os.IsNotExist(err) {
		return "No structured record for this visit; the shared stage log may belong to another visit.", nil
	}
	if err != nil {
		return "", err
	}
	if tape != nil {
		return renderRunTape(tape), nil
	}
	_, stale, graph := cli.GraphRunLog(tk, config.ExpandTilde(s.cfg.LogsDir), stage, run)
	if !graph || stale {
		return "No exact transcript for this attempt; the visit log may belong to a later retry.", nil
	}
	return content, nil
}

func renderRunTape(tape *logfmt.Tape) string {
	var b strings.Builder
	for _, event := range tape.Events {
		if event.Text != "" {
			b.WriteString(event.Text + "\n")
		}
		if event.Tool != "" {
			fmt.Fprintf(&b, "> %s %s\n%s\n", event.Tool, event.Arg, event.Result)
		}
	}
	return b.String()
}

var errDaemonNotRunning = fmt.Errorf("daemon not verified; actions and attachment unavailable in file mode")
var errAttachmentUnavailable = fmt.Errorf("verified daemon socket and session required for attachment")

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
	return app.New(app.Static(s.cfg), store.NewDiskRepo(s.cfg.TicketsDir), app.NoopRuntime{})
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
