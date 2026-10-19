package sender

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPoolKeepsPerKeyOrder(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			Node string `json:"node"`
			Seq  int    `json:"seq"`
		}
		_ = json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got[m.Node] = append(got[m.Node], m.Seq)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	p, err := NewPool(3, HTTPOptions{URL: srv.URL, Timeout: time.Second, QueueSize: 300}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)

	nodes := []string{"n1", "n2", "n3", "n4", "n5"}
	for seq := 1; seq <= 10; seq++ {
		for _, n := range nodes {
			p.EnqueueRaw(n, fmt.Sprint(seq), []byte(fmt.Sprintf(`{"node":%q,"seq":%d}`, n, seq)))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for p.Stats().Sent < 50 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s := p.Stats(); s.Sent != 50 || s.Pending != 0 || s.Dropped != 0 {
		t.Fatalf("stats = %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, n := range nodes {
		for i, seq := range got[n] {
			if seq != i+1 {
				t.Fatalf("%s arrived out of order: %v", n, got[n])
			}
		}
	}
}
