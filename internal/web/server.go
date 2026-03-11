package web

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"
)

//go:embed static/index.html
var staticFS embed.FS

type Server struct {
	svc      TicketService
	broker   *SSEBroker
	httpSrv  *http.Server
	log      *slog.Logger
	listener net.Listener
}

func New(svc TicketService, broker *SSEBroker, host string, port int, log *slog.Logger) *Server {
	s := &Server{
		svc:    svc,
		broker: broker,
		log:    log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/tickets", s.handleListTickets)
	mux.HandleFunc("POST /api/tickets", s.handleCreateTicket)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/tickets/{id}", s.handleGetTicket)
	mux.HandleFunc("DELETE /api/tickets/{id}", s.handleDeleteTicket)
	mux.HandleFunc("POST /api/tickets/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/tickets/{id}/retry", s.handleRetry)
	mux.HandleFunc("POST /api/tickets/{id}/skip", s.handleSkip)
	mux.HandleFunc("POST /api/tickets/{id}/move", s.handleMove)
	mux.HandleFunc("POST /api/tickets/{id}/init", s.handleInit)
	mux.HandleFunc("PUT /api/tickets/{id}", s.handleUpdateTicket)
	mux.HandleFunc("POST /api/tickets/upload", s.handleUploadTickets)
	mux.HandleFunc("GET /api/tickets/{id}/logs", s.handleGetLogs)
	mux.HandleFunc("GET /api/events", s.handleSSE)
	mux.HandleFunc("GET /ws/terminal/{id}", s.handleTerminalWS)
	subFS, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", http.FileServerFS(subFS))

	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("web server listen: %w", err)
	}
	s.listener = ln
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Error("web server error", "err", err)
		}
	}()
	return nil
}

func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}
