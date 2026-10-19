// Command klique-idle-hub is the per-cluster service the node collectors
// report to. It serves the latest state of every node as Prometheus metrics
// (for Grafana) and, only when an API key is set, forwards every report to
// Klique Cloud.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/envflag"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/hub"
	"github.com/klique-ai/idle-gpu-dashboard/collector/internal/sender"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// envPrefix: every flag can also be set as KLIQUE_IDLE_HUB_<FLAG>.
const envPrefix = "KLIQUE_IDLE_HUB_"

// DefaultCloudEndpoint is where Klique Cloud receives reports. Nothing is
// sent there unless --cloud-api-key is set.
const DefaultCloudEndpoint = "https://idle.klique.ai/v1/reports"

// drainTimeout is how long a stopping hub keeps forwarding queued reports.
const drainTimeout = 20 * time.Second

type options struct {
	listen, token, cloudEndpoint, cloudAPIKey, caFile string
	staleAfter, httpTimeout                           time.Duration
	queueSize, workers                                int
	dump, showVersion                                 bool
	logLevel, logFormat                               string
}

func main() { os.Exit(run()) }

func run() int {
	opt, err := parse(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if opt.showVersion {
		fmt.Println(version)
		return 0
	}
	log := newLogger(opt.logLevel, opt.logFormat)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var fwd *sender.Pool
	if opt.cloudAPIKey != "" {
		fwd, err = sender.NewPool(opt.workers, sender.HTTPOptions{
			URL:       opt.cloudEndpoint,
			Token:     opt.cloudAPIKey,
			Timeout:   opt.httpTimeout,
			CAFile:    opt.caFile,
			QueueSize: opt.queueSize,
			UserAgent: "klique-idle-hub/" + version,
		}, log)
		if err != nil {
			log.Error("cannot set up forwarding to Klique Cloud", "err", err)
			return 1
		}
	}
	hopt := hub.Options{Token: opt.token, StaleAfter: opt.staleAfter, Dump: opt.dump}
	if fwd != nil {
		hopt.Forward = fwd
	}
	h := hub.New(hopt, log)

	srv := &http.Server{Addr: opt.listen, Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second}
	cloud := "off (reports stay in the cluster)"
	if fwd != nil {
		cloud = opt.cloudEndpoint
	}
	log.Info("starting", "version", version, "listen", opt.listen, "token_set", opt.token != "", "cloud", cloud)

	senderCtx, stopSender := context.WithCancel(context.Background())
	senderDone := make(chan struct{})
	if fwd != nil {
		go func() { fwd.Run(senderCtx); close(senderDone) }()
	} else {
		close(senderDone)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		log.Error("cannot serve", "listen", opt.listen, "err", err)
		stopSender()
		return 1
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	// The forward queue lives in memory: give it time to reach Klique Cloud
	// (Kubernetes allows 30 s after SIGTERM by default).
	drain := time.Now().Add(drainTimeout)
	for fwd != nil && fwd.Stats().Pending > 0 && time.Now().Before(drain) {
		time.Sleep(100 * time.Millisecond)
	}
	if fwd != nil && fwd.Stats().Pending > 0 {
		log.Warn("exiting with reports not yet forwarded", "pending", fwd.Stats().Pending)
	}
	stopSender()
	<-senderDone
	log.Info("stopped")
	return 0
}

func parse(args []string, getenv func(string) string) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("klique-idle-hub", flag.ContinueOnError)
	var tokenFile, keyFile string
	fs.StringVar(&o.listen, "listen", ":8080", "address to serve on")
	fs.StringVar(&o.token, "token", "", "bearer token collectors must send; also required for GET /v1/nodes (prefer --token-file)")
	fs.StringVar(&tokenFile, "token-file", "", "file containing that token")
	fs.DurationVar(&o.staleAfter, "stale-after", 2*time.Minute, "forget nodes that haven't reported for this long")
	fs.StringVar(&o.cloudAPIKey, "cloud-api-key", "", "Klique Cloud API key; when set, every report is forwarded to --cloud-endpoint (prefer --cloud-api-key-file)")
	fs.StringVar(&keyFile, "cloud-api-key-file", "", "file containing the Klique Cloud API key")
	fs.StringVar(&o.cloudEndpoint, "cloud-endpoint", DefaultCloudEndpoint, "where Klique Cloud receives reports")
	fs.IntVar(&o.queueSize, "cloud-queue-size", 10000, "reports kept in memory while Klique Cloud is unreachable, in total (~3 KB each)")
	fs.IntVar(&o.workers, "cloud-workers", 4, "parallel connections to Klique Cloud; each node's reports stay in order")
	fs.DurationVar(&o.httpTimeout, "http-timeout", 10*time.Second, "timeout for one forwarding attempt")
	fs.StringVar(&o.caFile, "ca-file", "", "extra CA certificate(s) to trust for --cloud-endpoint (PEM), e.g. for a TLS-inspecting proxy")
	fs.BoolVar(&o.dump, "dump", false, "log every report as full JSON")
	fs.StringVar(&o.logLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&o.logFormat, "log-format", "text", "text or json")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: klique-idle-hub [flags]\n\n"+
			"Receives the reports of a cluster's collectors, serves them as Prometheus metrics\n"+
			"at /metrics, and forwards them to Klique Cloud only when --cloud-api-key is set.\n"+
			"Every flag can also be set as an environment variable, e.g. --stale-after as\n"+
			"%sSTALE_AFTER.\n\nFlags:\n", envPrefix)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := envflag.Apply(fs, envPrefix, getenv); err != nil {
		return nil, err
	}
	if o.showVersion {
		return o, nil
	}
	var err error
	if o.token, err = secret(o.token, tokenFile, "--token"); err != nil {
		return nil, err
	}
	if o.cloudAPIKey, err = secret(o.cloudAPIKey, keyFile, "--cloud-api-key"); err != nil {
		return nil, err
	}
	if u, err := url.Parse(o.cloudEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("--cloud-endpoint %q: must be an http(s) URL", o.cloudEndpoint)
	}
	if o.staleAfter < 10*time.Second {
		return nil, errors.New("--stale-after must be at least 10s")
	}
	if o.queueSize < 1 {
		return nil, errors.New("--cloud-queue-size must be at least 1")
	}
	if o.workers < 1 || o.workers > 64 {
		return nil, errors.New("--cloud-workers must be between 1 and 64")
	}
	return o, nil
}

// secret returns the value of a --x / --x-file flag pair.
func secret(value, file, name string) (string, error) {
	if file == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("set only one of %s and %s-file", name, name)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read %s-file: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func newLogger(lvl, format string) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(lvl)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
