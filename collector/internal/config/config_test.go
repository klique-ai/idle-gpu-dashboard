package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestEnvAndFlags(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("  from-file\n"), 0o600)

	c, err := Parse(
		[]string{"--report-interval=30s", "--labels", "site=sjc-1, rack=b4"},
		env(map[string]string{
			"KLIQUE_IDLE_CLUSTER":         "prod-a",
			"KLIQUE_IDLE_ENDPOINT":        "https://dash.example.com/v1/reports",
			"KLIQUE_IDLE_TOKEN_FILE":      tokenFile,
			"KLIQUE_IDLE_REPORT_INTERVAL": "99s", // flag wins
			"KLIQUE_IDLE_NODE_NAME":       "gpu-01",
			"KLIQUE_IDLE_REDACT":          "true",
		}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cluster != "prod-a" || c.NodeName != "gpu-01" || c.Token != "from-file" || !c.Redact {
		t.Fatalf("config = %+v", c)
	}
	if c.ReportInterval != 30*time.Second || c.SampleInterval != 2*time.Second {
		t.Fatalf("intervals = %v / %v", c.ReportInterval, c.SampleInterval)
	}
	if c.Labels["site"] != "sjc-1" || c.Labels["rack"] != "b4" {
		t.Fatalf("labels = %v", c.Labels)
	}
	if c.StdoutMode() {
		t.Fatal("endpoint set: not stdout mode")
	}
	if strings.Contains(strings.ToLower(strings.Join(func() []string {
		var s []string
		for k, v := range c.Redacted() {
			s = append(s, k+"="+toString(v))
		}
		return s
	}(), " ")), "from-file") {
		t.Fatal("Redacted() leaked the token")
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestValidation(t *testing.T) {
	cases := map[string][]string{
		"cluster required with endpoint": {"--endpoint=https://x"},
		"bad endpoint":                   {"--endpoint=ftp://x", "--cluster=a"},
		"report shorter than sample":     {"--sample-interval=10s", "--report-interval=5s"},
		"unknown gpu source":             {"--gpu=amd"},
		"unknown fake model":             {"--gpu=fake", "--fake-gpu-model=tpu"},
		"bad labels":                     {"--labels=novalue"},
		"token and token file":           {"--token=a", "--token-file=/nonexistent"},
		"stray argument":                 {"extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(args, env(nil), io.Discard); err == nil {
				t.Fatalf("Parse(%v) succeeded, want an error", args)
			}
		})
	}
}

func TestStdoutDefaults(t *testing.T) {
	c, err := Parse(nil, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !c.StdoutMode() || c.Cluster != "local" || c.NodeName == "" {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestBadEnvValue(t *testing.T) {
	_, err := Parse(nil, env(map[string]string{"KLIQUE_IDLE_SAMPLE_INTERVAL": "soon"}), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "KLIQUE_IDLE_SAMPLE_INTERVAL") {
		t.Fatalf("err = %v", err)
	}
}
