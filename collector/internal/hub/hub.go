// Package hub is the per-cluster service the node collectors report to. It
// keeps the latest report of every node, serves them as Prometheus metrics
// (and as JSON at /v1/nodes), and, only when configured, forwards every
// report to a Klique Cloud account.
package hub

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/report"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/sender"
)

const maxBody = 4 << 20

// Options configures a Hub.
type Options struct {
	// Token is the bearer token collectors must send. It also guards
	// GET /v1/nodes. Empty accepts any request.
	Token string
	// StaleAfter drops nodes that haven't reported for this long.
	StaleAfter time.Duration
	// Forward receives every accepted report, exactly as the collector sent
	// it; nil keeps everything in the cluster.
	Forward Forwarder
	// Dump logs every report as full JSON.
	Dump bool
}

// Forwarder delivers encoded reports, keeping each key's (node's) order.
type Forwarder interface {
	EnqueueRaw(key, id string, body []byte)
	Stats() sender.Stats
}

// Hub holds the latest report of every node.
type Hub struct {
	opt Options
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	nodes    map[string]entry // by cluster/node
	received uint64
	rejected uint64
}

type entry struct {
	rep  report.Report
	seen time.Time
}

// New returns a Hub.
func New(opt Options, log *slog.Logger) *Hub {
	if opt.StaleAfter <= 0 {
		opt.StaleAfter = 2 * time.Minute
	}
	return &Hub{opt: opt, log: log, now: time.Now, nodes: map[string]entry{}}
}

// Handler serves POST /v1/reports (any POST path), GET /metrics,
// GET /v1/nodes and GET /healthz.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"ok\":true,\"nodes\":%d}\n", len(h.fresh()))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		h.writeMetrics(w)
	})
	mux.HandleFunc("GET /v1/nodes", h.authorized(func(w http.ResponseWriter, _ *http.Request) {
		nodes := h.fresh()
		out := make([]report.Report, len(nodes))
		for i, e := range nodes {
			out[i] = e.rep
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	}))
	mux.HandleFunc("POST /", h.authorized(h.receive))
	return mux
}

func (h *Hub) receive(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			h.reject(w, http.StatusBadRequest, "bad gzip body: "+err.Error())
			return
		}
		defer zr.Close()
		body = zr
	}
	// Limit after decompression too, so a small gzip body can't expand
	// without bound.
	raw, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil {
		h.reject(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(raw) > maxBody {
		h.reject(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("report larger than %d bytes", maxBody))
		return
	}
	var rep report.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		h.reject(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if rep.Schema != report.Schema {
		h.reject(w, http.StatusUnprocessableEntity, fmt.Sprintf("unsupported schema %q; this hub accepts %s", rep.Schema, report.Schema))
		return
	}
	if rep.Node.Name == "" {
		h.reject(w, http.StatusBadRequest, "node.name is required")
		return
	}

	h.mu.Lock()
	h.nodes[rep.Cluster+"/"+rep.Node.Name] = entry{rep: rep, seen: h.now()}
	h.received++
	h.mu.Unlock()

	if h.opt.Forward != nil {
		// The bytes as received (gunzipped): fields this hub doesn't know
		// about reach the server too.
		h.opt.Forward.EnqueueRaw(rep.Cluster+"/"+rep.Node.Name, rep.ReportID, raw)
	}
	if h.opt.Dump {
		h.log.Info("report", "node", rep.Node.Name, "json", string(raw))
	} else {
		h.log.Debug("report", "cluster", rep.Cluster, "node", rep.Node.Name, "seq", rep.Collector.Seq)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Hub) reject(w http.ResponseWriter, code int, msg string) {
	h.mu.Lock()
	h.rejected++
	h.mu.Unlock()
	h.log.Warn("report rejected", "status", code, "err", msg)
	http.Error(w, msg, code)
}

// authorized rejects requests without the bearer token, when one is set.
func (h *Hub) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.opt.Token != "" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+h.opt.Token)) != 1 {
			if r.Method == http.MethodPost {
				// A collector with the wrong token: make it visible.
				h.reject(w, http.StatusUnauthorized, "bad or missing bearer token")
				return
			}
			http.Error(w, "bad or missing bearer token", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// fresh returns the nodes that reported within StaleAfter, sorted by key,
// and forgets the others.
func (h *Hub) fresh() []entry {
	cutoff := h.now().Add(-h.opt.StaleAfter)
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.nodes))
	for k, e := range h.nodes {
		if e.seen.Before(cutoff) {
			delete(h.nodes, k)
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]entry, len(keys))
	for i, k := range keys {
		out[i] = h.nodes[k]
	}
	return out
}

func (h *Hub) counters() (received, rejected uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.received, h.rejected
}
