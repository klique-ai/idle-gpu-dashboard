// Package config parses command-line flags. Every flag can also be set
// through an environment variable: --report-interval is
// KLIQUE_IDLE_REPORT_INTERVAL. Flags win over the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/clearml/klique-idle-dashboard/collector/internal/gpu"
)

// EnvPrefix prefixes the environment variable of every flag.
const EnvPrefix = "KLIQUE_IDLE_"

// Config is the collector configuration.
type Config struct {
	Cluster  string
	NodeName string
	Labels   map[string]string

	Endpoint           string
	Token              string
	Gzip               bool
	CAFile             string
	InsecureSkipVerify bool
	HTTPTimeout        time.Duration
	QueueSize          int

	SampleInterval time.Duration
	ReportInterval time.Duration

	GPU          string
	NVMLLibrary  string
	FakeGPUs     int
	FakeGPUModel string

	Scheduler         string
	SchedulerInterval time.Duration

	ProcRoot       string
	HostRoot       string
	Redact         bool
	IncludeCmdline bool

	HealthAddr string
	Once       bool
	Pretty     bool
	LogLevel   string
	LogFormat  string
	Version    bool
}

// StdoutMode reports whether reports go to stdout instead of a server.
func (c *Config) StdoutMode() bool { return c.Endpoint == "" || c.Once }

// Parse reads flags from args (without the program name) and the environment.
func Parse(args []string, getenv func(string) string, stderr io.Writer) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("klique-idle-collector", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var labels, tokenFile string
	fs.StringVar(&c.Cluster, "cluster", "", "cluster name shown on the dashboard (required when sending to an endpoint)")
	fs.StringVar(&c.NodeName, "node-name", "", "node name (default: hostname; on Kubernetes set from spec.nodeName)")
	fs.StringVar(&labels, "labels", "", "extra node labels sent with every report, e.g. site=sjc-1,rack=b4")

	fs.StringVar(&c.Endpoint, "endpoint", "", "URL to POST reports to; empty prints them to stdout")
	fs.StringVar(&c.Token, "token", "", "bearer token for the endpoint (prefer --token-file)")
	fs.StringVar(&tokenFile, "token-file", "", "file containing the bearer token")
	fs.BoolVar(&c.Gzip, "gzip", false, "gzip request bodies (the server must accept Content-Encoding: gzip)")
	fs.StringVar(&c.CAFile, "ca-file", "", "extra CA certificate(s) to trust for the endpoint (PEM)")
	fs.BoolVar(&c.InsecureSkipVerify, "insecure-skip-verify", false, "do not verify the endpoint's TLS certificate (testing only)")
	fs.DurationVar(&c.HTTPTimeout, "http-timeout", 10*time.Second, "timeout for one delivery attempt")
	fs.IntVar(&c.QueueSize, "queue-size", 360, "reports kept in memory while the endpoint is unreachable (360 = 1 h at 10 s)")

	fs.DurationVar(&c.SampleInterval, "sample-interval", 2*time.Second, "how often to read the node")
	fs.DurationVar(&c.ReportInterval, "report-interval", 10*time.Second, "how often to send a report")

	fs.StringVar(&c.GPU, "gpu", gpu.KindNVML, "GPU source: nvml, fake (simulated GPUs for demos and testing) or none")
	fs.StringVar(&c.NVMLLibrary, "nvml-library", "", "path to libnvidia-ml.so.1 if the loader can't find it")
	fs.IntVar(&c.FakeGPUs, "fake-gpus", 8, "number of simulated GPUs with --gpu=fake")
	fs.StringVar(&c.FakeGPUModel, "fake-gpu-model", "h100", "simulated GPU model: "+strings.Join(gpu.FakeModelNames(), ", "))

	fs.StringVar(&c.Scheduler, "scheduler", "auto", "workload scheduler: auto, kubernetes, slurm, pbs or none")
	fs.DurationVar(&c.SchedulerInterval, "scheduler-interval", 30*time.Second, "how often to ask the scheduler for job names and allocations")

	fs.StringVar(&c.ProcRoot, "proc-root", "/proc", "procfs mount")
	fs.StringVar(&c.HostRoot, "host-root", "/", "where the host's root filesystem is mounted (used for os-release)")
	fs.BoolVar(&c.Redact, "redact", false, "replace user, job, pod and process names with hashes")
	fs.BoolVar(&c.IncludeCmdline, "include-cmdline", false, "send GPU process command lines (may contain secrets passed as arguments)")

	fs.StringVar(&c.HealthAddr, "health-addr", "127.0.0.1:9478", "address for the /healthz endpoint; \"off\" disables it")
	fs.BoolVar(&c.Once, "once", false, "take one short sample, print the report to stdout and exit (shows exactly what would be sent)")
	fs.BoolVar(&c.Pretty, "pretty", false, "indent JSON printed to stdout")
	fs.StringVar(&c.LogLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", "text", "text or json")
	fs.BoolVar(&c.Version, "version", false, "print the version and exit")

	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: klique-idle-collector [flags]\n\n"+
			"Reports GPU, CPU and memory usage of this node to a Klique Idle Dashboard server.\n"+
			"Every flag can also be set as an environment variable, e.g. --report-interval\n"+
			"as %sREPORT_INTERVAL.\n\nFlags:\n", EnvPrefix)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := applyEnv(fs, getenv); err != nil {
		return nil, err
	}
	if c.Version {
		return c, nil
	}

	var err error
	if c.Labels, err = parseLabels(labels); err != nil {
		return nil, err
	}
	if tokenFile != "" {
		if c.Token != "" {
			return nil, errors.New("set only one of --token and --token-file")
		}
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if c.NodeName == "" {
		if c.NodeName, err = os.Hostname(); err != nil {
			return nil, fmt.Errorf("node name: %w", err)
		}
	}
	return c, c.validate()
}

// applyEnv sets every flag not given on the command line from its
// environment variable.
func applyEnv(fs *flag.FlagSet, getenv func(string) string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || err != nil {
			return
		}
		env := EnvPrefix + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if v := getenv(env); v != "" {
			if e := fs.Set(f.Name, v); e != nil {
				err = fmt.Errorf("%s=%q: %w", env, v, e)
			}
		}
	})
	return err
}

func (c *Config) validate() error {
	var errs []error
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("--endpoint %q: must be an http(s) URL", c.Endpoint))
		}
		if c.Cluster == "" && !c.Once {
			errs = append(errs, errors.New("--cluster is required when sending to an endpoint"))
		}
	}
	if c.Cluster == "" {
		c.Cluster = "local"
	}
	if strings.EqualFold(c.HealthAddr, "off") {
		c.HealthAddr = ""
	}
	if c.SampleInterval < 100*time.Millisecond {
		errs = append(errs, errors.New("--sample-interval must be at least 100ms"))
	}
	if c.ReportInterval < c.SampleInterval {
		errs = append(errs, errors.New("--report-interval must not be shorter than --sample-interval"))
	}
	switch c.GPU {
	case gpu.KindNVML, gpu.KindNone:
	case gpu.KindFake:
		if _, ok := gpu.FakeModels[c.FakeGPUModel]; !ok {
			errs = append(errs, fmt.Errorf("--fake-gpu-model %q: want one of %s", c.FakeGPUModel, strings.Join(gpu.FakeModelNames(), ", ")))
		}
		if c.FakeGPUs < 1 || c.FakeGPUs > 64 {
			errs = append(errs, errors.New("--fake-gpus must be between 1 and 64"))
		}
	default:
		errs = append(errs, fmt.Errorf("--gpu %q: want nvml, fake or none", c.GPU))
	}
	switch c.Scheduler {
	case "auto", "kubernetes", "slurm", "pbs", "none":
	default:
		errs = append(errs, fmt.Errorf("--scheduler %q: want auto, kubernetes, slurm, pbs or none", c.Scheduler))
	}
	if c.SchedulerInterval < time.Second {
		errs = append(errs, errors.New("--scheduler-interval must be at least 1s"))
	}
	if c.QueueSize < 1 {
		errs = append(errs, errors.New("--queue-size must be at least 1"))
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("--log-format %q: want text or json", c.LogFormat))
	}
	return errors.Join(errs...)
}

func parseLabels(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("--labels: %q is not key=value", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// Redacted returns a printable copy of the settings with the token masked.
func (c *Config) Redacted() map[string]any {
	keys := map[string]any{
		"cluster": c.Cluster, "node": c.NodeName, "endpoint": c.Endpoint, "gpu": c.GPU,
		"sample_interval": c.SampleInterval.String(),
		"report_interval": c.ReportInterval.String(), "redact": c.Redact, "token_set": c.Token != "",
	}
	if len(c.Labels) > 0 {
		var l []string
		for k, v := range c.Labels {
			l = append(l, k+"="+v)
		}
		sort.Strings(l)
		keys["labels"] = strings.Join(l, ",")
	}
	return keys
}
