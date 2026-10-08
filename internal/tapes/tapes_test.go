package tapes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/capture"
	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/config"
)

func decode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildTurnStreamingClaude(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	x := capture.Exchange{
		RequestID:    "abc",
		SourceFormat: "claude",
		Model:        "claude-sonnet-5-5",
		Stream:       true,
		RequestHeaders: http.Header{
			"User-Agent":               {"claude-cli/2.1.0 (external, cli)"},
			"X-Claude-Code-Session-Id": {"sess-1"},
			"X-Claude-Code-Agent-Id":   {"agent-7"},
		},
		Request:     []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Response:    []byte(sse),
		StatusCode:  200,
		StartedAt:   time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 10, 8, 10, 0, 2, 0, time.UTC),
	}
	raw, err := BuildTurn(x, "cliproxyapi")
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, raw)

	if got["provider"] != "anthropic" || got["agent_name"] != "cliproxyapi" || got["raw_response_encoding"] != "identity" {
		t.Errorf("turn = %v", got)
	}
	if _, has := got["response"]; has {
		t.Error("reduced response must be omitted so tapes reduces raw_response")
	}
	if b, _ := base64.StdEncoding.DecodeString(got["raw_response"].(string)); string(b) != sse {
		t.Errorf("raw_response = %q", b)
	}
	if req := got["request"].(map[string]any); req["model"] != "claude-sonnet-5-5" {
		t.Errorf("request not embedded verbatim: %v", req)
	}
	meta := got["meta"].(map[string]any)
	want := map[string]any{
		"request_id": "cliproxyapi-abc", "content_type": "text/event-stream", "stream": "true",
		"thread_id": "agent-7", "upstream_status": 200.0, "elapsed_seconds": 2.0,
		"ts_request": "2026-10-08T10:00:00Z", "captured_at": "2026-10-08T10:00:02Z",
	}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("meta[%s] = %v, want %v", k, meta[k], v)
		}
	}
	sess := got["session"].(map[string]any)
	if sess["harness_id"] != "claude" || sess["harness_session_id"] != "sess-1" {
		t.Errorf("session = %v", sess)
	}
}

func TestBuildTurnProviders(t *testing.T) {
	for format, want := range map[string]string{"openai": "openai", "openai-response": "openai", "claude": "anthropic"} {
		raw, err := BuildTurn(capture.Exchange{SourceFormat: format, Request: []byte(`{}`), Response: []byte(`{}`)}, "a")
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if got := decode(t, raw)["provider"]; got != want {
			t.Errorf("%s -> %v, want %s", format, got, want)
		}
	}
	for _, format := range []string{"gemini", "gemini-cli", ""} {
		if _, err := BuildTurn(capture.Exchange{SourceFormat: format, Request: []byte(`{}`)}, "a"); err == nil {
			t.Errorf("%q: expected unsupported error", format)
		}
	}
	if _, err := BuildTurn(capture.Exchange{SourceFormat: "openai", Request: []byte(`not json`)}, "a"); err == nil {
		t.Error("expected error for non-JSON request")
	}
}

func TestIdentifySession(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	cases := []struct {
		name                   string
		headers                http.Header
		body                   string
		harness, sessionID, ag string
	}{
		{
			name:    "explicit tapes headers win",
			headers: http.Header{"X-Tapes-Harness-Id": {"mine"}, "X-Tapes-Harness-Session-Id": {"s1"}, "X-Tapes-Agent-Name": {"bot"}, "X-Claude-Code-Session-Id": {"ignored"}},
			body:    body, harness: "mine", sessionID: "s1", ag: "bot",
		},
		{
			name:    "claude code header",
			headers: http.Header{"X-Claude-Code-Session-Id": {"cc-1"}},
			body:    body, harness: "claude", sessionID: "cc-1",
		},
		{
			name: "claude code metadata json",
			body: `{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"cc-2\"}"}}`, harness: "claude", sessionID: "cc-2",
		},
		{
			name: "claude code legacy metadata",
			body: `{"metadata":{"user_id":"user_ab12_account__session_01a06a06-e830-7da9-a866-98470a94389c"}}`, harness: "claude", sessionID: "01a06a06-e830-7da9-a866-98470a94389c",
		},
		{
			name:    "codex session header",
			headers: http.Header{"session_id": {"cx-1"}, "User-Agent": {"codex_cli_rs/0.50.0"}},
			body:    `{"input":[]}`, harness: "codex", sessionID: "cx-1",
		},
		{
			name:    "prompt cache key",
			headers: http.Header{"User-Agent": {"codex_cli_rs/0.50.0"}},
			body:    `{"prompt_cache_key":"pck-1","input":[]}`, harness: "codex", sessionID: "pck-1",
		},
		{
			name: "unknown client falls back to fingerprint",
			body: body, harness: "cliproxyapi", sessionID: conversationFingerprint([]byte(body)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := identifySession(tc.headers, []byte(tc.body))
			if s.HarnessID != tc.harness || s.HarnessSessionID != tc.sessionID || s.AgentName != tc.ag {
				t.Errorf("got %+v", s)
			}
		})
	}
}

func TestConversationFingerprint(t *testing.T) {
	first := `{"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`
	later := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`
	other := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"different"}]}`

	a, b, c := conversationFingerprint([]byte(first)), conversationFingerprint([]byte(later)), conversationFingerprint([]byte(other))
	if !strings.HasPrefix(a, "cpa-") || len(a) != 20 {
		t.Errorf("fingerprint format %q", a)
	}
	if a != b {
		t.Errorf("same conversation fingerprinted differently: %s vs %s", a, b)
	}
	if a == c {
		t.Error("different conversations share a fingerprint")
	}
}

type ingestRecorder struct {
	mu      sync.Mutex
	bodies  []string
	headers []http.Header
	status  int
}

func (r *ingestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path != "/v1/ingest" || req.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	r.bodies = append(r.bodies, string(b))
	r.headers = append(r.headers, req.Header.Clone())
	w.WriteHeader(r.status)
}

func TestSenderDelivers(t *testing.T) {
	rec := &ingestRecorder{status: http.StatusAccepted}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	cfg, _ := config.Parse([]byte("ingest-url: " + srv.URL + "\nheaders:\n  Authorization: Bearer t\n"))
	s := NewSender(cfg, func(string, string, map[string]any) {})
	s.Enqueue([]byte(`{"n":1}`))
	s.Enqueue([]byte(`{"n":2}`))
	s.Close(context.Background())

	if len(rec.bodies) != 2 {
		t.Fatalf("delivered %d turns, want 2", len(rec.bodies))
	}
	if h := rec.headers[0]; h.Get("Content-Type") != "application/json" || h.Get("Authorization") != "Bearer t" {
		t.Errorf("headers = %v", h)
	}
	if sent, failed, dropped := s.Stats(); sent != 2 || failed != 0 || dropped != 0 {
		t.Errorf("stats = %d/%d/%d", sent, failed, dropped)
	}
	s.Enqueue([]byte(`{"after":"close"}`)) // must not panic
}

func TestSenderLogsFailureOnce(t *testing.T) {
	rec := &ingestRecorder{status: http.StatusUnprocessableEntity}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	var mu sync.Mutex
	var warns int
	cfg, _ := config.Parse([]byte("ingest-url: " + srv.URL))
	s := NewSender(cfg, func(level, _ string, _ map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		if level == "warn" {
			warns++
		}
	})
	for range 5 {
		s.Enqueue([]byte(`{}`))
	}
	s.Close(context.Background())

	if _, failed, _ := s.Stats(); failed != 5 {
		t.Errorf("failed = %d, want 5", failed)
	}
	if warns != 1 {
		t.Errorf("warnings = %d, want 1", warns)
	}
}

func TestSenderDropsWhenFull(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	cfg, _ := config.Parse([]byte("ingest-url: " + srv.URL + "\nqueue-size: 1\n"))
	s := NewSender(cfg, func(string, string, map[string]any) {})
	for range 20 {
		s.Enqueue([]byte(`{}`))
	}
	close(block)
	s.Close(context.Background())

	sent, _, dropped := s.Stats()
	if sent+dropped != 20 || dropped == 0 {
		t.Errorf("sent=%d dropped=%d", sent, dropped)
	}
}
