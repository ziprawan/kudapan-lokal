// Package webhook serves HTTP webhook endpoints for Telegram updates and
// Discord HTTP Interactions alongside the background watcher.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const maxWebhookBodyBytes int64 = 1 << 20

var errBodyTooLarge = errors.New("request body exceeds 1 MiB limit")

// Config configures the HTTP webhook server and its platform handlers.
type Config struct {
	ListenAddr      string
	TelegramHandler http.Handler
	DiscordHandler  http.Handler
	Logger          *log.Logger
}

// Server exposes health and platform webhook routes over HTTP.
type Server struct {
	cfg    Config
	mux    *http.ServeMux
	logger *log.Logger

	mu   sync.RWMutex
	addr net.Addr
}

// NewServer constructs a webhook Server with /api/healthz, /api/webhook/telegram,
// and /api/webhook/discord routes registered.
func NewServer(cfg Config) (*Server, error) {
	cfg.ListenAddr = strings.TrimSpace(cfg.ListenAddr)
	if cfg.ListenAddr == "" {
		return nil, errors.New("webhook listen address cannot be empty")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("POST /api/webhook/telegram", handlerOrUnavailable(cfg.TelegramHandler, "telegram webhook not configured"))
	mux.Handle("POST /api/webhook/discord", handlerOrUnavailable(cfg.DiscordHandler, "discord webhook not configured"))

	return &Server{
		cfg:    cfg,
		mux:    mux,
		logger: logger,
	}, nil
}

// Handler returns the Server's HTTP router for direct testing or embedding.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// Addr returns the bound network address while the server is running, or nil before Run listens.
func (s *Server) Addr() net.Addr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.addr
}

// Run listens on cfg.ListenAddr and serves HTTP requests until ctx is canceled,
// then performs a bounded graceful shutdown.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen webhook server on %q: %w", s.cfg.ListenAddr, err)
	}

	s.mu.Lock()
	s.addr = ln.Addr()
	s.mu.Unlock()

	srv := &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	s.logger.Printf("webhook server listening on %s (routes: GET /api/healthz, POST /api/webhook/telegram, POST /api/webhook/discord)", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("webhook server failed: %w", err)
		}
		return nil
	case <-ctx.Done():
		s.logger.Printf("webhook server shutting down: %v", ctx.Err())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		shutdownErr := srv.Shutdown(shutdownCtx)
		serveErr := <-errCh
		if shutdownErr != nil {
			return fmt.Errorf("shutdown webhook server: %w", shutdownErr)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("webhook server stopped with error: %w", serveErr)
		}
		return nil
	}
}

func handlerOrUnavailable(h http.Handler, msg string) http.Handler {
	if h != nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": msg})
	})
}

func readLimitedBody(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errBodyTooLarge
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
