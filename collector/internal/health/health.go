// Package health serves /healthz for liveness probes and humans, and /metrics
// (this node's latest report in Prometheus format) for scrapers.
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
	"github.com/clearml/klique-idle-dashboard/collector/internal/metrics"
	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
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

// Source is what the server reads from the collector.
type Source interface {
	Status() collector.Status
	LastReport() *report.Report
}

// New builds the server. The loop counts as stuck when no sample was taken
// for maxStale.
func New(addr string, c Source, s sender.Sender, maxStale time.Duration, log *slog.Logger) *Server {
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
	// The same series the hub serves for a whole cluster, for this node only,
	// so Prometheus can scrape nodes directly (bare metal, Slurm, PBS).
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		m := metrics.NewSet()
		// While the sampling loop is stalled the last report is stale: leave the
		// node's series out (a gap in Grafana) rather than repeat old values.
		last := c.Status().LastSample
		if r := c.LastReport(); r != nil && !last.IsZero() && time.Since(last) <= maxStale {
			m.AddReport(r)
		}
		st := s.Stats()
		m.Gauge("klique_idle_collector_reports_pending", "Reports waiting to be delivered to the endpoint.", nil, float64(st.Pending))
		m.Counter("klique_idle_collector_reports_dropped_total", "Reports dropped instead of delivered (queue full or rejected).", nil, float64(st.Dropped))
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.Write(w)
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
