package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeTapes records turns POSTed to /v1/ingest.
type fakeTapes struct {
	mu    sync.Mutex
	turns []map[string]any
}

func (f *fakeTapes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/ingest" {
		http.NotFound(w, r)
		return
	}
	var turn map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &turn)
	f.mu.Lock()
	f.turns = append(f.turns, turn)
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"accepted"}`))
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// call sends a host-shaped request. The host encodes pluginapi structs with
// encoding/json, so keys are Go field names and []byte fields are base64.
func call(t *testing.T, p *plugin, method string, payload map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(payload)
	result, err := p.handle(method, raw)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	out, _ := json.Marshal(result)
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m
}

func TestPluginRecordsStreamAndNonStreamTurns(t *testing.T) {
	tapes := &fakeTapes{}
	srv := httptest.NewServer(tapes)
	defer srv.Close()

	p := newPlugin(func(string, string, map[string]any) {})
	reg := call(t, p, "plugin.register", map[string]any{
		"config_yaml":    b64("enabled: true\ningest-url: " + srv.URL + "\nagent-name: homelab\n"),
		"schema_version": 6,
	})
	if reg["schema_version"] != 6.0 {
		t.Errorf("schema_version = %v", reg["schema_version"])
	}
	caps := reg["capabilities"].(map[string]any)
	for _, c := range []string{"response_interceptor", "response_stream_interceptor", "request_lifecycle_plugin"} {
		if caps[c] != true {
			t.Errorf("capability %s not declared", c)
		}
	}

	// Streaming Chat Completions request.
	headers := map[string]any{"X-Claude-Code-Session-Id": []string{"cc-session"}}
	call(t, p, "response.intercept_stream_chunk", map[string]any{
		"RequestID": "req-stream", "SourceFormat": "openai", "Model": "gpt-x", "ChunkIndex": -1,
		"RequestHeaders": headers, "OriginalRequest": b64(`{"model":"gpt-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	})
	for i, chunk := range []string{`{"choices":[{"delta":{"content":"he"}}]}`, `{"choices":[{"delta":{"content":"llo"}}]}`} {
		res := call(t, p, "response.intercept_stream_chunk", map[string]any{"RequestID": "req-stream", "ChunkIndex": i, "Body": b64(chunk)})
		if len(res) != 0 {
			t.Errorf("chunk interceptor must pass through unchanged, returned %v", res)
		}
	}
	call(t, p, "request.complete", map[string]any{
		"RequestID": "req-stream", "Outcome": "succeeded", "StatusCode": 200,
		"StartedAt": "2026-10-08T10:00:00Z", "CompletedAt": "2026-10-08T10:00:01Z",
	})

	// Non-streaming Claude request.
	call(t, p, "response.intercept_after", map[string]any{
		"RequestID": "req-json", "SourceFormat": "claude", "Model": "claude-x", "StatusCode": 200,
		"OriginalRequest": b64(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`),
		"Body":            b64(`{"type":"message","role":"assistant","content":[{"type":"text","text":"hello"}]}`),
	})
	call(t, p, "request.complete", map[string]any{"RequestID": "req-json", "Outcome": "succeeded", "StatusCode": 200})

	// Gemini-protocol clients are not representable in tapes and are skipped.
	call(t, p, "response.intercept_after", map[string]any{
		"RequestID": "req-gemini", "SourceFormat": "gemini", "OriginalRequest": b64(`{}`), "Body": b64(`{}`),
	})
	call(t, p, "request.complete", map[string]any{"RequestID": "req-gemini", "Outcome": "succeeded"})

	p.shutdown()

	if len(tapes.turns) != 2 {
		t.Fatalf("tapes received %d turns, want 2", len(tapes.turns))
	}
	byID := map[string]map[string]any{}
	for _, turn := range tapes.turns {
		byID[turn["meta"].(map[string]any)["request_id"].(string)] = turn
	}

	stream := byID["cliproxyapi-req-stream"]
	if stream["provider"] != "openai" || stream["agent_name"] != "homelab" {
		t.Errorf("stream turn = %v", stream)
	}
	sse, _ := base64.StdEncoding.DecodeString(stream["raw_response"].(string))
	wantSSE := "data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n"
	if string(sse) != wantSSE {
		t.Errorf("raw_response = %q, want %q", sse, wantSSE)
	}
	if s := stream["session"].(map[string]any); s["harness_id"] != "claude" || s["harness_session_id"] != "cc-session" {
		t.Errorf("session = %v", s)
	}

	nonStream := byID["cliproxyapi-req-json"]
	if nonStream["provider"] != "anthropic" || nonStream["meta"].(map[string]any)["content_type"] != "application/json" {
		t.Errorf("non-stream turn = %v", nonStream)
	}
}

func TestPluginKeepsRegisteringWithInvalidConfig(t *testing.T) {
	var errors int
	p := newPlugin(func(level, _ string, _ map[string]any) {
		if level == "error" {
			errors++
		}
	})
	reg := call(t, p, "plugin.register", map[string]any{"config_yaml": b64("ingest-url: not a url\n"), "schema_version": 2})
	if reg["schema_version"] != 2.0 || reg["metadata"] == nil {
		t.Errorf("registration = %v", reg)
	}
	if errors != 1 || p.sender.Load() != nil {
		t.Errorf("errors=%d sender=%v", errors, p.sender.Load())
	}
	p.shutdown()
}

func TestDispatchUnknownMethod(t *testing.T) {
	raw, err := dispatch("model.static", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Errorf("envelope = %s", raw)
	}
}

func TestPluginReconfigure(t *testing.T) {
	p := newPlugin(func(string, string, map[string]any) {})
	defer p.shutdown()
	register := func(yaml string) {
		call(t, p, "plugin.reconfigure", map[string]any{"config_yaml": b64(yaml), "schema_version": 6})
	}

	register("ingest-url: http://a:8082\n")
	first := p.sender.Load()
	register("enabled: true\ningest-url: http://a:8082\n")
	if p.sender.Load() != first {
		t.Error("unchanged config replaced the sender")
	}
	register("ingest-url: http://b:8082\n")
	if p.sender.Load() == first {
		t.Error("changed config kept the old sender")
	}
	register("ingest-url: ::bad\n")
	if p.config.IngestEndpoint != "http://b:8082/v1/ingest" {
		t.Errorf("invalid config replaced settings: %s", p.config.IngestEndpoint)
	}
}

func TestPluginResumesAfterAbortedUnload(t *testing.T) {
	p := newPlugin(func(string, string, map[string]any) {})
	cfg := map[string]any{"config_yaml": b64("ingest-url: http://a:8082\n"), "schema_version": 6}
	call(t, p, "plugin.register", cfg)
	call(t, p, "plugin.quiesce", nil)
	if p.sender.Load() != nil {
		t.Fatal("quiesce left the sender running")
	}
	call(t, p, "plugin.reconfigure", cfg)
	if p.sender.Load() == nil {
		t.Error("reconfigure after quiesce did not resume recording")
	}
	p.shutdown()
}
