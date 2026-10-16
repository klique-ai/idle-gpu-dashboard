// Command klique-idle-collector runs on every node and reports GPU, CPU and
// memory usage to a Klique Idle Dashboard server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/collector"
	"github.com/clearml/klique-idle-dashboard/collector/internal/config"
	"github.com/clearml/klique-idle-dashboard/collector/internal/gpu"
	"github.com/clearml/klique-idle-dashboard/collector/internal/health"
	"github.com/clearml/klique-idle-dashboard/collector/internal/host"
	"github.com/clearml/klique-idle-dashboard/collector/internal/owner"
	"github.com/clearml/klique-idle-dashboard/collector/internal/scheduler"
	"github.com/clearml/klique-idle-dashboard/collector/internal/sender"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const drainTimeout = 5 * time.Second

func main() { os.Exit(run()) }

func run() int {
	cfg, err := config.Parse(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if cfg.Version {
		fmt.Println(version)
		return 0
	}
	log := newLogger(cfg)

	schedKind := cfg.Scheduler
	if schedKind == "auto" {
		schedKind = scheduler.Detect(cfg.ProcRoot)
	}

	var src gpu.Source
	switch cfg.GPU {
	case gpu.KindFake:
		src = gpu.NewFake(cfg.FakeGPUs, gpu.FakeModels[cfg.FakeGPUModel], cfg.Cluster+"/"+cfg.NodeName, schedKind)
		log.Warn("using SIMULATED GPUs: reports are marked gpu_source=fake")
	case gpu.KindNone:
		src = gpu.None{}
	default:
		src = gpu.NewNVML(cfg.NVMLLibrary, log)
	}
	defer src.Close()

	watcher := newSchedulerWatcher(schedKind, cfg, log)

	var out sender.Sender
	if cfg.StdoutMode() {
		out = sender.NewStdout(os.Stdout, cfg.Pretty || cfg.Once)
	} else {
		h, err := sender.NewHTTP(sender.HTTPOptions{
			URL:                cfg.Endpoint,
			Token:              cfg.Token,
			Gzip:               cfg.Gzip,
			Timeout:            cfg.HTTPTimeout,
			CAFile:             cfg.CAFile,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
			QueueSize:          cfg.QueueSize,
			UserAgent:          "klique-idle-collector/" + version,
		}, log)
		if err != nil {
			log.Error("cannot set up delivery", "err", err)
			return 1
		}
		if strings.HasPrefix(cfg.Endpoint, "http://") && cfg.Token != "" {
			log.Warn("sending the token over plain HTTP; use https for anything but local testing")
		}
		if cfg.InsecureSkipVerify {
			log.Warn("TLS certificate verification is disabled")
		}
		out = h
	}

	c := collector.New(collector.Options{
		Cluster:        cfg.Cluster,
		NodeName:       cfg.NodeName,
		Labels:         cfg.Labels,
		SampleInterval: cfg.SampleInterval,
		ReportInterval: cfg.ReportInterval,
		Version:        version,
		Redact:         cfg.Redact,
		SchedulerKind:  schedKind,
	}, src, host.New(cfg.ProcRoot, cfg.HostRoot), owner.NewResolver(cfg.ProcRoot, cfg.IncludeCmdline), watcher, out, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Once {
		if watcher != nil {
			watcher.Refresh(ctx)
		}
		c.Run(ctx, true)
		return 0
	}

	attrs := []any{"version", version, "scheduler", schedKind}
	for k, v := range cfg.Redacted() {
		attrs = append(attrs, k, v)
	}
	log.Info("starting", attrs...)

	if cfg.HealthAddr != "" {
		maxStale := 3*cfg.SampleInterval + 10*time.Second
		hs := health.New(cfg.HealthAddr, c, out, maxStale, log)
		if err := hs.Start(); err != nil {
			hint := ""
			if errors.Is(err, syscall.EADDRINUSE) {
				hint = "another collector is probably already running on this node; use --health-addr to pick another port, or --health-addr=off"
			}
			log.Error("cannot start health endpoint", "addr", cfg.HealthAddr, "err", err, "hint", hint)
			return 1
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			hs.Stop(sctx)
		}()
	}

	senderCtx, stopSender := context.WithCancel(context.Background())
	senderDone := make(chan struct{})
	go func() { out.Run(senderCtx); close(senderDone) }()
	if watcher != nil {
		go watcher.Run(ctx)
	}

	c.Run(ctx, false) // returns on SIGTERM/SIGINT after queueing the last window

	// Give queued reports a moment to go out before exiting.
	deadline := time.Now().Add(drainTimeout)
	for out.Stats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := out.Stats().Pending; n > 0 {
		log.Warn("exiting with undelivered reports", "pending", n)
	}
	stopSender()
	<-senderDone
	log.Info("stopped")
	return 0
}

func newSchedulerWatcher(kind string, cfg *config.Config, log *slog.Logger) *scheduler.Watcher {
	var src scheduler.Source
	switch kind {
	case scheduler.Kubernetes:
		k, err := scheduler.NewKubernetes(cfg.NodeName)
		if err != nil {
			log.Warn("Kubernetes integration disabled: pod names and GPU allocation won't be reported", "err", err)
			return nil
		}
		src = k
	case scheduler.Slurm:
		// Slurm node names are short hostnames.
		short, _, _ := strings.Cut(cfg.NodeName, ".")
		src = scheduler.NewSlurm(short)
	case scheduler.PBS:
		src = scheduler.NewPBS(cfg.NodeName)
	default:
		return nil
	}
	return scheduler.NewWatcher(src, cfg.SchedulerInterval, log)
}

func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	// Logs go to stderr: stdout carries reports in stdout mode.
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
