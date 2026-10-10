package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/capture"
	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/config"
	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/tapes"
)

// pluginVersion is set at release build time with -ldflags "-X main.pluginVersion=...".
var pluginVersion = "0.0.0-dev"

const (
	repository = "https://github.com/lachieh/cliproxyapi-plugin-tapes"
	logo       = "https://raw.githubusercontent.com/lachieh/cliproxyapi-plugin-tapes/main/assets/icon.svg"

	// maxSchemaVersion is the newest host plugin schema this plugin understands.
	// From schema 3 the host omits request bodies from stream payload chunks,
	// which the recorder handles by caching them from the header-init call.
	maxSchemaVersion = 6

	// staleAfter bounds how long a request is tracked without request.complete.
	staleAfter    = 10 * time.Minute
	sweepInterval = time.Minute
	closeTimeout  = 5 * time.Second
)

// plugin holds everything the host's calls share. Host calls arrive
// concurrently from many request goroutines.
type plugin struct {
	log      tapes.Logger
	recorder *capture.Recorder
	now      func() time.Time

	sender atomic.Pointer[tapes.Sender]
	names  atomic.Pointer[tapes.Names]

	configMu sync.Mutex
	config   *config.Config

	startSweeper sync.Once
}

func newPlugin(log tapes.Logger) *plugin {
	return &plugin{log: log, recorder: capture.NewRecorder(), now: time.Now}
}

func (p *plugin) handle(method string, request []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML    []byte `json:"config_yaml"`
			SchemaVersion int    `json:"schema_version"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		p.configure(req.ConfigYAML)
		return registration(req.SchemaVersion), nil

	case "response.intercept_after":
		var ev capture.ResponseIntercept
		if err := json.Unmarshal(request, &ev); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		p.recorder.ObserveResponse(ev, p.now())
		return struct{}{}, nil

	case "response.intercept_stream_chunk":
		var ev capture.StreamChunk
		if err := json.Unmarshal(request, &ev); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		p.recorder.ObserveStreamChunk(ev, p.now())
		return struct{}{}, nil

	case "request.complete":
		var ev capture.Completion
		if err := json.Unmarshal(request, &ev); err != nil {
			return nil, fmt.Errorf("decode %s: %w", method, err)
		}
		if x, ok := p.recorder.Complete(ev); ok {
			p.record(x)
		}
		return struct{}{}, nil

	case "plugin.quiesce", "plugin.shutdown":
		p.shutdown()
		return struct{}{}, nil

	default:
		return nil, errUnknownMethod{method}
	}
}

type errUnknownMethod struct{ method string }

func (e errUnknownMethod) Error() string { return "unknown method " + e.method }

// configure applies a new config. An invalid config is logged and the
// previous one kept, so the plugin stays registered and its settings stay
// editable from the management UI. The host reconfigures on every config
// file change, so an unchanged config is a no-op.
func (p *plugin) configure(raw []byte) {
	cfg, err := config.Parse(raw)
	if err != nil {
		p.log("error", "tapes: invalid config, keeping previous settings", map[string]any{"error": err.Error()})
		return
	}
	p.configMu.Lock()
	defer p.configMu.Unlock()
	if p.config != nil && reflect.DeepEqual(*p.config, cfg) {
		return
	}
	p.config = &cfg
	p.names.Store(&tapes.Names{Keys: cfg.KeyNames, FallbackAgent: cfg.FallbackAgentName})
	if old := p.sender.Swap(tapes.NewSender(cfg, p.log)); old != nil {
		go closeSender(old)
	}
	p.log("info", "tapes: recording sessions", map[string]any{"ingest_url": cfg.IngestEndpoint})
	p.startSweeper.Do(func() { go p.sweep() })
}

func (p *plugin) record(x capture.Exchange) {
	sender := p.sender.Load()
	if sender == nil {
		return
	}
	var names tapes.Names
	if n := p.names.Load(); n != nil {
		names = *n
	}
	turn, err := tapes.BuildTurn(x, names)
	if err != nil {
		p.log("debug", "tapes: skipping request", map[string]any{"request_id": x.RequestID, "reason": err.Error()})
		return
	}
	sender.Enqueue(turn)
}

// sweep runs for the life of the process; with no sender it only discards.
func (p *plugin) sweep() {
	for range time.Tick(sweepInterval) {
		for _, x := range p.recorder.Sweep(p.now().Add(-staleAfter)) {
			p.record(x)
		}
	}
}

func (p *plugin) shutdown() {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	// Forget the config so a reconfigure after an aborted unload starts recording again.
	p.config = nil
	if s := p.sender.Swap(nil); s != nil {
		closeSender(s)
		sent, failed, dropped := s.Stats()
		p.log("info", "tapes: stopped", map[string]any{"sent": sent, "failed": failed, "dropped": dropped})
	}
}

func closeSender(s *tapes.Sender) {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	s.Close(ctx)
}

type configField struct {
	Name        string
	Type        string
	Description string
}

func registration(hostSchemaVersion int) map[string]any {
	schema := min(max(hostSchemaVersion, 1), maxSchemaVersion)
	return map[string]any{
		"schema_version": schema,
		"metadata": map[string]any{
			"Name":             "tapes",
			"Version":          pluginVersion,
			"Author":           "Lachlan Heywood",
			"GitHubRepository": repository,
			"Logo":             logo,
			"ConfigFields": []configField{
				{"ingest-url", "string", "tapes ingest API URL, e.g. http://tapes.lan:8082. /v1/ingest is appended when missing. Default: " + config.DefaultIngestURL},
				{"keys", "object", "Names for client API keys (name: key), recorded as the session's auth subject and in the agent name, e.g. laptop/claude."},
				{"headers", "object", "Extra HTTP headers sent to tapes, e.g. an Authorization header for a gateway in front of it."},
				{"timeout-seconds", "integer", "Timeout for each ingest request. Default: 10"},
				{"queue-size", "integer", "Turns buffered while tapes is slow; beyond this they are dropped. Default: 256"},
			},
		},
		"capabilities": map[string]bool{
			"response_interceptor":        true,
			"response_stream_interceptor": true,
			"request_lifecycle_plugin":    true,
		},
	}
}
