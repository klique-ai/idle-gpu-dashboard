// Package sender delivers reports: to an HTTP(S) endpoint with a bounded
// retry queue, or as JSON lines to stdout.
package sender

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

// Sender accepts finished reports.
type Sender interface {
	Enqueue(r *report.Report)
	// Run delivers queued reports until ctx is cancelled.
	Run(ctx context.Context)
	Stats() Stats
}

// Stats describes delivery health.
type Stats struct {
	Pending     int       `json:"pending"`
	Sent        uint64    `json:"sent"`
	Dropped     uint64    `json:"dropped"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
}

// HTTPOptions configures an HTTP sender.
type HTTPOptions struct {
	URL                string
	Token              string
	Gzip               bool
	Timeout            time.Duration
	CAFile             string
	InsecureSkipVerify bool
	QueueSize          int
	UserAgent          string
}

// HTTP POSTs each report as one JSON document. Delivery is at-least-once:
// a retried report keeps its report_id so the receiver can de-duplicate.
type HTTP struct {
	opt    HTTPOptions
	client *http.Client
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) bool

	mu       sync.Mutex
	queue    []*item // oldest first
	stats    Stats
	failures int
	notify   chan struct{}
}

// NewHTTP builds an HTTP sender. It honours HTTPS_PROXY/NO_PROXY.
func NewHTTP(opt HTTPOptions, log *slog.Logger) (*HTTP, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opt.InsecureSkipVerify} //nolint:gosec // opt-in for testing
	if opt.CAFile != "" {
		pem, err := os.ReadFile(opt.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file %s: no certificates found", opt.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	if opt.QueueSize < 1 {
		opt.QueueSize = 1
	}
	return &HTTP{
		opt:    opt,
		client: &http.Client{Timeout: opt.Timeout, Transport: tr},
		log:    log,
		sleep:  sleepCtx,
		notify: make(chan struct{}, 1),
	}, nil
}

// item is one encoded report waiting to be sent.
type item struct {
	id   string // report_id, for logs
	body []byte
}

// Enqueue adds a report, dropping the oldest one when the queue is full.
func (h *HTTP) Enqueue(r *report.Report) {
	body, err := json.Marshal(r)
	if err != nil { // can't happen with report.Report
		h.log.Error("cannot encode report", "err", err)
		return
	}
	h.EnqueueRaw(r.ReportID, body)
}

// EnqueueRaw adds an already encoded report, sent exactly as given. The hub
// uses it to forward collector reports unchanged.
func (h *HTTP) EnqueueRaw(id string, body []byte) {
	h.mu.Lock()
	if len(h.queue) >= h.opt.QueueSize {
		h.queue[0] = nil
		h.queue = h.queue[1:]
		h.stats.Dropped++
		if h.stats.Dropped == 1 || h.stats.Dropped%100 == 0 {
			h.log.Warn("send queue full, dropping oldest report", "queue_size", h.opt.QueueSize, "dropped_total", h.stats.Dropped)
		}
	}
	h.queue = append(h.queue, &item{id: id, body: body})
	h.mu.Unlock()
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

// Stats implements Sender.
func (h *HTTP) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.stats
	s.Pending = len(h.queue)
	return s
}

// Run implements Sender.
func (h *HTTP) Run(ctx context.Context) {
	for {
		r := h.head()
		if r == nil {
			select {
			case <-ctx.Done():
				return
			case <-h.notify:
				continue
			}
		}
		err := h.post(ctx, r)
		if ctx.Err() != nil {
			return
		}
		var perm *permanentError
		switch {
		case err == nil:
			h.done(r, false)
			h.recordSuccess()
		case errors.As(err, &perm):
			h.done(r, true)
			h.recordFailure(err, true)
		default:
			n := h.recordFailure(err, false)
			if !h.sleep(ctx, backoff(n)) {
				return
			}
		}
	}
}

func (h *HTTP) head() *item {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) == 0 {
		return nil
	}
	return h.queue[0]
}

// done removes it from the queue (unless it was already pushed out by Enqueue).
func (h *HTTP) done(it *item, rejected bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	queued := len(h.queue) > 0 && h.queue[0] == it
	if queued {
		h.queue[0] = nil
		h.queue = h.queue[1:]
	}
	switch {
	case !rejected:
		h.stats.Sent++
	case queued: // an evicted report was already counted as dropped by Enqueue
		h.stats.Dropped++
	}
}

func (h *HTTP) recordSuccess() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failures > 0 {
		h.log.Info("delivery recovered", "after_failures", h.failures)
	}
	h.failures = 0
	h.stats.LastSuccess = time.Now()
}

func (h *HTTP) recordFailure(err error, rejected bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stats.LastError, h.stats.LastErrorAt = err.Error(), time.Now()
	if rejected {
		// The server answered: the connection is fine, so this doesn't feed
		// the retry backoff.
		h.log.Error("report rejected by server, dropping it", "err", err)
		return h.failures
	}
	h.failures++
	if h.failures == 1 || h.failures%20 == 0 {
		h.log.Warn("delivery failed, will retry", "err", err, "consecutive_failures", h.failures, "pending", len(h.queue))
	}
	return h.failures
}

type permanentError struct{ error }

func (h *HTTP) post(ctx context.Context, it *item) error {
	body := it.body
	if h.opt.Gzip {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.opt.URL, bytes.NewReader(body))
	if err != nil {
		return &permanentError{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", h.opt.UserAgent)
	if h.opt.Gzip {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if h.opt.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.opt.Token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	err = fmt.Errorf("POST %s: %s: %s", h.opt.URL, resp.Status, strings.TrimSpace(string(msg)))
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		// The server can't accept this report; retrying the same bytes won't help.
		return &permanentError{err}
	}
	return err // 401/403/404 (misconfiguration that may get fixed), 408, 429, 5xx
}

// backoff grows 1s, 2s, 4s... to 60s, with ±20% jitter so a fleet of
// collectors doesn't retry in lockstep after a server outage.
func backoff(failures int) time.Duration {
	d := time.Second << min(failures-1, 6)
	d = min(d, time.Minute)
	jitter := 0.8 + 0.4*rand.Float64() //nolint:gosec // not security sensitive
	return time.Duration(float64(d) * jitter)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Stdout writes each report as one JSON line (or indented, for humans).
type Stdout struct {
	mu     sync.Mutex
	w      io.Writer
	indent bool
	sent   uint64
}

// NewStdout writes to w.
func NewStdout(w io.Writer, indent bool) *Stdout { return &Stdout{w: w, indent: indent} }

// Enqueue implements Sender; it writes immediately.
func (s *Stdout) Enqueue(r *report.Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	enc := json.NewEncoder(s.w)
	if s.indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(r); err == nil {
		s.sent++
	}
}

// Run implements Sender.
func (s *Stdout) Run(ctx context.Context) { <-ctx.Done() }

// Stats implements Sender.
func (s *Stdout) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Sent: s.sent, LastSuccess: time.Now()}
}
