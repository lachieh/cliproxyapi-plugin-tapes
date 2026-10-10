// Package config parses the plugin's section of the CLIProxyAPI config
// (plugins.configs.tapes).
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultIngestURL is where `tapes serve` exposes its ingest API out of the box.
const DefaultIngestURL = "http://localhost:8082"

const ingestPath = "/v1/ingest"

type Config struct {
	// IngestEndpoint is the full URL turns are POSTed to.
	IngestEndpoint string
	// KeyNames maps a client API key's caller scope to the name it is recorded under.
	KeyNames  map[string]string
	Headers   map[string]string
	Timeout   time.Duration
	QueueSize int
}

type rawConfig struct {
	IngestURL      string            `yaml:"ingest-url"`
	Keys           map[string]string `yaml:"keys"`
	Headers        map[string]string `yaml:"headers"`
	TimeoutSeconds *int              `yaml:"timeout-seconds"`
	QueueSize      *int              `yaml:"queue-size"`
}

// Parse reads the plugin config YAML and applies defaults. Unknown keys,
// including the host-owned `enabled` and `priority`, are ignored.
func Parse(data []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	endpoint, err := ingestEndpoint(raw.IngestURL)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		IngestEndpoint: endpoint,
		Headers:        raw.Headers,
		Timeout:        10 * time.Second,
		QueueSize:      256,
	}
	for name, key := range raw.Keys {
		name, key = strings.TrimSpace(name), strings.TrimSpace(key)
		if name == "" || key == "" {
			return Config{}, fmt.Errorf("keys: entries need a name and a key")
		}
		if cfg.KeyNames == nil {
			cfg.KeyNames = make(map[string]string, len(raw.Keys))
		}
		cfg.KeyNames[CallerScope(key)] = name
	}
	if raw.TimeoutSeconds != nil {
		if *raw.TimeoutSeconds <= 0 {
			return Config{}, fmt.Errorf("timeout-seconds must be positive, got %d", *raw.TimeoutSeconds)
		}
		cfg.Timeout = time.Duration(*raw.TimeoutSeconds) * time.Second
	}
	if raw.QueueSize != nil {
		if *raw.QueueSize <= 0 {
			return Config{}, fmt.Errorf("queue-size must be positive, got %d", *raw.QueueSize)
		}
		cfg.QueueSize = *raw.QueueSize
	}
	return cfg, nil
}

// ingestEndpoint accepts either a base URL (http://tapes.lan:8082) or the
// full ingest URL, and returns the full ingest URL.
func ingestEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultIngestURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("ingest-url %q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("ingest-url %q must be an absolute http(s) URL", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, ingestPath) {
		u.Path += ingestPath
	}
	return u.String(), nil
}

// CallerScope reproduces CLIProxyAPI's caller scope for a client API key
// (sdk/cliproxy/session.CallerScope), so keys can be named without the
// plugin ever handling the key a request used.
func CallerScope(apiKey string) string {
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:])
}
