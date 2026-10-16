// Command mock-receiver is a stand-in for the dashboard server, for local
// development and tests. It accepts reports on POST (any path), prints a
// one-line summary of each, and serves the latest report per node at
// GET /v1/nodes. It keeps everything in memory.
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/report"
)

const maxBody = 4 << 20

type store struct {
	mu     sync.Mutex
	latest map[string]report.Report // by cluster/node
	count  int
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	token := flag.String("token", os.Getenv("MOCK_RECEIVER_TOKEN"), "require this bearer token (default $MOCK_RECEIVER_TOKEN; empty accepts any)")
	dump := flag.Bool("dump", false, "print every report in full")
	flag.Parse()

	s := &store{latest: map[string]report.Report{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, `{"ok":true}`)
	})
	mux.HandleFunc("GET /v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		keys := make([]string, 0, len(s.latest))
		for k := range s.latest {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]report.Report, 0, len(keys))
		for _, k := range keys {
			out = append(out, s.latest[k])
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		if *token != "" && r.Header.Get("Authorization") != "Bearer "+*token {
			http.Error(w, "bad or missing bearer token", http.StatusUnauthorized)
			return
		}
		var body io.Reader = http.MaxBytesReader(w, r.Body, maxBody)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(body)
			if err != nil {
				http.Error(w, "bad gzip body: "+err.Error(), http.StatusBadRequest)
				return
			}
			defer zr.Close()
			body = zr
		}
		raw, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		var rep report.Report
		if err := json.Unmarshal(raw, &rep); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if rep.Schema != report.Schema {
			http.Error(w, fmt.Sprintf("unsupported schema %q", rep.Schema), http.StatusUnprocessableEntity)
			return
		}
		s.mu.Lock()
		s.latest[rep.Cluster+"/"+rep.Node.Name] = rep
		s.count++
		n := s.count
		s.mu.Unlock()

		log.Printf("#%d %s %s", n, r.URL.Path, summary(rep))
		if *dump {
			pretty, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(pretty))
		}
		w.WriteHeader(http.StatusAccepted)
	})

	log.Printf("mock receiver listening on %s (token required: %v)", *addr, *token != "")
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// summary renders a report as one line: who, when, and each GPU's state.
func summary(r report.Report) string {
	var gpus []string
	for _, g := range r.GPUs {
		state := "?"
		if g.UtilPercent != nil {
			state = fmt.Sprintf("%.0f%%", g.UtilPercent.Avg)
		}
		if g.MemoryUsedBytes != nil && g.MemoryTotalBytes > 0 {
			state += fmt.Sprintf("/%.0f%%mem", 100*float64(g.MemoryUsedBytes.Max)/float64(g.MemoryTotalBytes))
		}
		if len(g.Processes) > 0 {
			p := g.Processes[0]
			who := p.Name
			if p.Owner != nil {
				who = p.Owner.Kind + ":" + firstNonEmpty(p.Owner.Name, p.Owner.ID)
			}
			state += "(" + who + ")"
		}
		gpus = append(gpus, state)
	}
	host := ""
	if r.Host != nil {
		host = fmt.Sprintf(" cpu=%.0f%%", r.Host.CPUUtilPercent.Avg)
	}
	errs := ""
	if len(r.Errors) > 0 {
		errs = fmt.Sprintf(" errors=%d", len(r.Errors))
	}
	return fmt.Sprintf("%s/%s seq=%d src=%s window=%s+%.0fs%s gpus=[%s]%s",
		r.Cluster, r.Node.Name, r.Collector.Seq, r.Collector.GPUSource,
		r.Window.Start.Format("15:04:05"), r.Window.End.Sub(r.Window.Start).Seconds(),
		host, strings.Join(gpus, " "), errs)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
