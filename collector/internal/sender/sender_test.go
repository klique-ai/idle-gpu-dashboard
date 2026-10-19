package sender

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type receiver struct {
	mu       sync.Mutex
	statuses []int // status to return per request, last one repeats
	got      []report.Report
	headers  []http.Header
	calls    int
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	status := rc.statuses[min(rc.calls, len(rc.statuses)-1)]
	rc.calls++
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		body = zr
	}
	var rep report.Report
	if err := json.NewDecoder(body).Decode(&rep); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	rc.headers = append(rc.headers, r.Header.Clone())
	if status < 300 {
		rc.got = append(rc.got, rep)
	}
	w.WriteHeader(status)
}

func (rc *receiver) received() []report.Report {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]report.Report(nil), rc.got...)
}

func newSender(t *testing.T, url string, opt HTTPOptions) *HTTP {
	t.Helper()
	opt.URL, opt.Timeout, opt.UserAgent = url, 5*time.Second, "test/1"
	if opt.QueueSize == 0 {
		opt.QueueSize = 10
	}
	h, err := NewHTTP(opt, quiet)
	if err != nil {
		t.Fatal(err)
	}
	h.sleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil } // no real backoff in tests
	return h
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func rep(id string) *report.Report { return &report.Report{Schema: report.Schema, ReportID: id} }

func TestDeliversWithAuthAndGzip(t *testing.T) {
	rc := &receiver{statuses: []int{202}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	h := newSender(t, srv.URL, HTTPOptions{Token: "s3cret", Gzip: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	h.Enqueue(rep("a"))
	h.Enqueue(rep("b"))
	waitFor(t, func() bool { return len(rc.received()) == 2 })
	got := rc.received()
	if got[0].ReportID != "a" || got[1].ReportID != "b" {
		t.Fatalf("order = %s, %s", got[0].ReportID, got[1].ReportID)
	}
	hd := rc.headers[0]
	if hd.Get("Authorization") != "Bearer s3cret" || hd.Get("Content-Type") != "application/json" || hd.Get("User-Agent") != "test/1" {
		t.Fatalf("headers = %v", hd)
	}
	waitFor(t, func() bool { return h.Stats().Sent == 2 })
	if s := h.Stats(); s.Pending != 0 || s.Dropped != 0 || s.LastSuccess.IsZero() {
		t.Fatalf("stats = %+v", s)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	rc := &receiver{statuses: []int{503, 500, 429, 200}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	h := newSender(t, srv.URL, HTTPOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	h.Enqueue(rep("x"))
	waitFor(t, func() bool { return len(rc.received()) == 1 })
	if got := rc.received()[0].ReportID; got != "x" {
		t.Fatalf("delivered %q", got)
	}
	waitFor(t, func() bool { return h.Stats().Sent == 1 })
}

func TestDropsRejectedReports(t *testing.T) {
	rc := &receiver{statuses: []int{422, 200}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	h := newSender(t, srv.URL, HTTPOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	h.Enqueue(rep("bad"))
	h.Enqueue(rep("good"))
	waitFor(t, func() bool { return len(rc.received()) == 1 })
	if got := rc.received()[0].ReportID; got != "good" {
		t.Fatalf("delivered %q, want good", got)
	}
	waitFor(t, func() bool { return h.Stats().Dropped == 1 })
	if !strings.Contains(h.Stats().LastError, "422") {
		t.Fatalf("last error = %q", h.Stats().LastError)
	}
}

func TestQueueDropsOldestWhenFull(t *testing.T) {
	h := newSender(t, "http://127.0.0.1:1", HTTPOptions{QueueSize: 2}) // never started
	h.Enqueue(rep("1"))
	h.Enqueue(rep("2"))
	h.Enqueue(rep("3"))
	if s := h.Stats(); s.Pending != 2 || s.Dropped != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if h.head().id != "2" {
		t.Fatalf("head = %s, want 2", h.head().id)
	}
}

func TestRejectedReportsCountedOnceAndDontSlowRetries(t *testing.T) {
	h := newSender(t, "http://127.0.0.1:1", HTTPOptions{QueueSize: 1})
	h.Enqueue(rep("1"))
	inFlight := h.head()
	h.Enqueue(rep("2")) // queue full: evicts the in-flight report, Dropped=1
	h.done(inFlight, true)
	if s := h.Stats(); s.Dropped != 1 || s.Pending != 1 {
		t.Fatalf("stats = %+v, want dropped=1 pending=1", s)
	}
	if n := h.recordFailure(&permanentError{errors.New("422")}, true); n != 0 {
		t.Fatalf("rejection bumped the consecutive-failure count to %d", n)
	}
	if n := h.recordFailure(errors.New("timeout"), false); n != 1 {
		t.Fatalf("transient failure count = %d, want 1", n)
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: time.Second, 3: 4 * time.Second, 7: time.Minute, 50: time.Minute} {
		got := backoff(n)
		if got < want*8/10 || got > want*12/10 {
			t.Errorf("backoff(%d) = %v, want ~%v", n, got, want)
		}
	}
}

func TestStdout(t *testing.T) {
	var buf bytes.Buffer
	s := NewStdout(&buf, false)
	s.Enqueue(rep("a"))
	s.Enqueue(rep("b"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[1], `"report_id":"b"`) {
		t.Fatalf("output = %q", buf.String())
	}
	if s.Stats().Sent != 2 {
		t.Fatalf("sent = %d", s.Stats().Sent)
	}
}
