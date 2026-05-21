package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spore-host/spawn/pkg/observability"
)

// Server provides HTTP endpoints for metrics
type Server struct {
	config     observability.MetricsConfig
	registry   *Registry
	httpServer *http.Server
}

// NewServer creates a new metrics Server
func NewServer(config observability.MetricsConfig, registry *Registry) *Server {
	mux := http.NewServeMux()

	s := &Server{
		config:   config,
		registry: registry,
	}

	// Metrics endpoint
	mux.Handle(config.Path, promhttp.HandlerFor(
		registry.Gatherer(),
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		},
	))

	// Health endpoint
	mux.HandleFunc("/health", s.handleHealth)

	// State endpoint
	mux.HandleFunc("/state", s.handleState)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", config.Bind, config.Port),
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  15 * time.Second,
	}

	return s
}

// Start starts the metrics server
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		log.Printf("Metrics server listening on %s", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("Metrics server shutdown error: %v", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-time.After(100 * time.Millisecond):
		return nil
	}
}

// Shutdown gracefully shuts down the server
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	state := map[string]interface{}{
		"server": "spored",
		"time":   time.Now().UTC().Format(time.RFC3339),
		"config": map[string]interface{}{
			"enabled": s.config.Enabled,
			"port":    s.config.Port,
			"path":    s.config.Path,
			"bind":    s.config.Bind,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}
