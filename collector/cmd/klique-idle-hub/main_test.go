package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	env := map[string]string{"KLIQUE_IDLE_HUB_STALE_AFTER": "5m", "KLIQUE_IDLE_HUB_TOKEN": "t"}
	o, err := parse(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if o.staleAfter.Minutes() != 5 || o.token != "t" {
		t.Errorf("env not applied: %+v", o)
	}
	if o.cloudAPIKey != "" || o.cloudEndpoint != DefaultCloudEndpoint {
		t.Errorf("cloud must be off by default: key %q endpoint %q", o.cloudAPIKey, o.cloudEndpoint)
	}

	key := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(key, []byte("kq_a_b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err = parse([]string{"--cloud-api-key-file", key}, func(string) string { return "" })
	if err != nil || o.cloudAPIKey != "kq_a_b" {
		t.Errorf("key file: %q, %v", o.cloudAPIKey, err)
	}

	for _, args := range [][]string{
		{"--cloud-api-key", "x", "--cloud-api-key-file", key},
		{"--cloud-endpoint", "ftp://x"},
		{"--stale-after", "1s"},
	} {
		if _, err := parse(args, func(string) string { return "" }); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}
