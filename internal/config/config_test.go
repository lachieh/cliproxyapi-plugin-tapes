package config

import (
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("enabled: true\npriority: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IngestEndpoint != "http://localhost:8082/v1/ingest" {
		t.Errorf("endpoint = %q", cfg.IngestEndpoint)
	}
	if cfg.AgentName != "cliproxyapi" || cfg.Timeout != 10*time.Second || cfg.QueueSize != 256 {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestParseIngestURL(t *testing.T) {
	cases := map[string]string{
		"http://tapes.lan:8082":                "http://tapes.lan:8082/v1/ingest",
		"https://tapes.example.com/":           "https://tapes.example.com/v1/ingest",
		"https://gw.example.com/tapes":         "https://gw.example.com/tapes/v1/ingest",
		"http://tapes.lan:8082/v1/ingest":      "http://tapes.lan:8082/v1/ingest",
		"  http://tapes.lan:8082/v1/ingest/  ": "http://tapes.lan:8082/v1/ingest",
	}
	for in, want := range cases {
		cfg, err := Parse([]byte("ingest-url: \"" + in + "\""))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if cfg.IngestEndpoint != want {
			t.Errorf("%q -> %q, want %q", in, cfg.IngestEndpoint, want)
		}
	}
}

func TestParseOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
agent-name: homelab
timeout-seconds: 3
queue-size: 8
headers:
  Authorization: Bearer abc
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentName != "homelab" || cfg.Timeout != 3*time.Second || cfg.QueueSize != 8 || cfg.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseRejectsInvalid(t *testing.T) {
	for _, in := range []string{
		"ingest-url: tapes.lan:8082",
		"ingest-url: ftp://tapes.lan",
		"timeout-seconds: 0",
		"queue-size: -1",
		"headers: [1, 2]",
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}
