// Package health serves /healthz for liveness probes and humans.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/collector"
	"github.com/clearml/klique-idle-dashboard/collector/internal/sender"
)

// Server reports the collector as healthy while its sampling loop runs.
type Server struct {
	srv *http.Server
	log *slog.Logger
}

type response struct {
	OK       bool             `json:"ok"`
	Problem  string           `json:"problem,omitempty"`
	Status   collector.Status `json:"status"`
	Delivery sender.Stats     `json:"delivery"`
}

// New builds the server. The loop counts as stuck when no sample was taken
// for maxStale.
func New(addr string, c *collector.Collector, s sender.Sender, maxStale time.Duration, log *slog.Logger) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		resp := response{OK: true, Status: c.Status(), Delivery: s.Stats()}
		if last := resp.Status.LastSample; last.IsZero() || time.Since(last) > maxStale {
			resp.OK, resp.Problem = false, "sampling loop stalled"
		}
		w.Header().Set("Content-Type", "application/json")
		if !resp.OK {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	})
	return &Server{
		srv: &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second},
		log: log,
	}
}

// Start listens in the background. It fails fast if the port is taken.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.srv.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("health server stopped", "err", err)
		}
	}()
	s.log.Info("health endpoint listening", "addr", ln.Addr().String())
	return nil
}

// Stop shuts the server down.
func (s *Server) Stop(ctx context.Context) { _ = s.srv.Shutdown(ctx) }
