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

//go:embed static
var staticFS embed.FS

type Server struct {
	svc      TicketService
	broker   *SSEBroker
	httpSrv  *http.Server
	log      *slog.Logger
	listener net.Listener
	token    string
}

func New(svc TicketService, broker *SSEBroker, host string, port int, token string, log *slog.Logger) *Server {
	s := &Server{
		svc:    svc,
		broker: broker,
		log:    log,
		token:  token,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/tickets", s.handleListTickets)
	mux.HandleFunc("POST /api/tickets", s.handleCreateTicket)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/config/raw", s.handleGetRawConfig)
	mux.HandleFunc("PUT /api/config/raw", s.handlePutRawConfig)
	mux.HandleFunc("GET /api/tickets/{id}", s.handleGetTicket)
	mux.HandleFunc("DELETE /api/tickets/{id}", s.handleDeleteTicket)
	mux.HandleFunc("POST /api/tickets/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/tickets/{id}/retry", s.handleRetry)
	mux.HandleFunc("POST /api/tickets/{id}/run", s.handleRun)
	mux.HandleFunc("POST /api/tickets/{id}/skip", s.handleSkip)
	mux.HandleFunc("POST /api/tickets/{id}/set-stage", s.handleSetStage)
	mux.HandleFunc("POST /api/tickets/{id}/move", s.handleMove)
	mux.HandleFunc("POST /api/tickets/{id}/note", s.handleAddNote)
	mux.HandleFunc("POST /api/tickets/{id}/init", s.handleInit)
	mux.HandleFunc("PUT /api/tickets/{id}", s.handleUpdateTicket)
	mux.HandleFunc("POST /api/tickets/upload", s.handleUploadTickets)
	mux.HandleFunc("GET /api/tickets/{id}/logs", s.handleGetLogs)
	mux.HandleFunc("POST /api/tickets/{id}/plannotator-review", s.handlePlannotatorReview)
	mux.HandleFunc("GET /api/events", s.handleSSE)
	mux.HandleFunc("GET /ws/terminal/{id}", s.handleTerminalWS)
	subFS, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /", s.staticHandler(http.FileServerFS(subFS)))

	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           authMiddleware(s.token, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// staticHandler serves the embedded UI. When a token is configured and the
// request carries a ?token= that matches it, it sets the kontora_token cookie
// so subsequent same-origin fetch/EventSource/WebSocket calls (which cannot set
// headers) authenticate automatically. Only a valid token writes the cookie, so
// a /?token=garbage link cannot overwrite a working cookie. The cookie is
// HttpOnly and SameSite=Lax; Secure is set whenever the browser connection is
// HTTPS, including behind a TLS-terminating proxy (via X-Forwarded-Proto). On
// plain HTTP it stays unset, since marking it Secure there would make the
// browser drop it and break the tailnet-over-HTTP flow. A MaxAge makes it a
// persistent cookie so the token survives browser restarts instead of being
// cleared at session end.
//
// After setting the cookie it redirects to the same URL with the token query
// param removed, so the token is not retained in browser history, server logs,
// or the Referer header.
func (s *Server) staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			if q := r.URL.Query().Get("token"); tokenMatches(s.token, q) {
				http.SetCookie(w, &http.Cookie{
					Name:     tokenCookieName,
					Value:    q,
					Path:     "/",
					MaxAge:   int(tokenCookieMaxAge.Seconds()),
					HttpOnly: true,
					Secure:   isHTTPS(r),
					SameSite: http.SameSiteLaxMode,
				})
				vals := r.URL.Query()
				vals.Del("token")
				dest := r.URL.Path
				if enc := vals.Encode(); enc != "" {
					dest += "?" + enc
				}
				http.Redirect(w, r, dest, http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
