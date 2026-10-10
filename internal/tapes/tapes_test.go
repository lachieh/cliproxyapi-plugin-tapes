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
		CallerScope:  config.CallerScope("sk-laptop"),
		Model:        "claude-sonnet-5-5",
		Stream:       true,
		RequestHeaders: http.Header{
			"User-Agent":               {"claude-cli/2.1.0 (external, cli)"},
			"X-Claude-Code-Session-Id": {"sess-1"},
			"X-Claude-Code-Agent-Id":   {"agent-7"},
		},
		Request:     []byte(`{"model":"claude-sonnet-5-5","system":[{"type":"text","text":"Env:\nPrimary working directory: /src/myproj\nIs git repo: yes"}],"messages":[{"role":"user","content":"hi"}],"stream":true}`),
		Response:    []byte(sse),
		StatusCode:  200,
		StartedAt:   time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 10, 8, 10, 0, 2, 0, time.UTC),
	}
	raw, err := BuildTurn(x, Names{Keys: map[string]string{config.CallerScope("sk-laptop"): "laptop"}})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, raw)

	if got["provider"] != "anthropic" || got["agent_name"] != "laptop/claude" || got["raw_response_encoding"] != "identity" {
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
	wantSession := map[string]any{
		"auth_subject": "laptop", "harness_id": "claude", "harness_session_id": "sess-1",
		"harness_version": "2.1.0", "cwd": "/src/myproj",
	}
	for k, v := range wantSession {
		if sess[k] != v {
			t.Errorf("session[%s] = %v, want %v", k, sess[k], v)
		}
	}
}

func TestBuildTurnProviders(t *testing.T) {
	for format, want := range map[string]string{"openai": "openai", "openai-response": "openai", "claude": "anthropic"} {
		raw, err := BuildTurn(capture.Exchange{SourceFormat: format, Request: []byte(`{}`), Response: []byte(`{}`)}, Names{})
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if got := decode(t, raw)["provider"]; got != want {
			t.Errorf("%s -> %v, want %s", format, got, want)
		}
	}
	for _, format := range []string{"gemini", "gemini-cli", ""} {
		if _, err := BuildTurn(capture.Exchange{SourceFormat: format, Request: []byte(`{}`)}, Names{}); err == nil {
			t.Errorf("%q: expected unsupported error", format)
		}
	}
	if _, err := BuildTurn(capture.Exchange{SourceFormat: "openai", Request: []byte(`not json`)}, Names{}); err == nil {
		t.Error("expected error for non-JSON request")
	}
}

func TestIdentify(t *testing.T) {
	laptop := config.CallerScope("sk-laptop")
	names := Names{Keys: map[string]string{laptop: "laptop"}, FallbackAgent: "cpa"}
	claudeUA := http.Header{"User-Agent": {"claude-cli/2.1.0 (external, cli)"}}
	cases := []struct {
		name    string
		headers http.Header
		body    string
		scope   string
		want    identity
	}{
		{
			name:    "named key and known tool",
			headers: http.Header{"User-Agent": {"claude-cli/2.1.0 (external, cli)"}, "X-Claude-Code-Session-Id": {"cc-1"}},
			scope:   laptop,
			want:    identity{AgentName: "laptop/claude", AuthSubject: "laptop", HarnessID: "claude", HarnessVersion: "2.1.0", HarnessSessionID: "cc-1"},
		},
		{
			name:    "known tool, unnamed key",
			headers: http.Header{"User-Agent": {"codex_cli_rs/0.50.0 (Mac OS 15.0; arm64)"}, "session_id": {"cx-1"}},
			body:    `{"input":[{"role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/home/me/proj</cwd>\n</environment_context>"}]}]}`,
			scope:   config.CallerScope("sk-other"),
			want:    identity{AgentName: "codex", HarnessID: "codex", HarnessVersion: "0.50.0", HarnessSessionID: "cx-1", Cwd: "/home/me/proj"},
		},
		{
			name:  "named key, unknown tool",
			body:  `{"messages":[{"role":"user","content":"hi"}]}`,
			scope: laptop,
			want:  identity{AgentName: "laptop", AuthSubject: "laptop", HarnessID: "cliproxyapi", HarnessSessionID: conversationFingerprint([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))},
		},
		{
			name: "nothing known",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: identity{AgentName: "cpa", HarnessID: "cliproxyapi", HarnessSessionID: conversationFingerprint([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))},
		},
		{
			name:    "claude session from metadata json",
			headers: claudeUA,
			body:    `{"metadata":{"user_id":"{\"device_id\":\"d\",\"session_id\":\"cc-2\"}"}}`,
			want:    identity{AgentName: "claude", HarnessID: "claude", HarnessVersion: "2.1.0", HarnessSessionID: "cc-2"},
		},
		{
			name:    "claude session from legacy metadata",
			headers: claudeUA,
			body:    `{"metadata":{"user_id":"user_ab12_account__session_01a06a06-e830-7da9-a866-98470a94389c"}}`,
			want:    identity{AgentName: "claude", HarnessID: "claude", HarnessVersion: "2.1.0", HarnessSessionID: "01a06a06-e830-7da9-a866-98470a94389c"},
		},
		{
			name:    "codex prompt cache key",
			headers: http.Header{"User-Agent": {"codex_cli_rs/0.50.0"}},
			body:    `{"prompt_cache_key":"pck-1","input":[]}`,
			want:    identity{AgentName: "codex", HarnessID: "codex", HarnessVersion: "0.50.0", HarnessSessionID: "pck-1"},
		},
		{
			name:    "opencode working directory",
			headers: http.Header{"User-Agent": {"opencode/0.15.2"}},
			body:    `{"messages":[{"role":"system","content":"<env>\n  Working directory: /work/site\n</env>"}]}`,
			want:    identity{AgentName: "opencode", HarnessID: "opencode", HarnessVersion: "0.15.2", Cwd: "/work/site", HarnessSessionID: conversationFingerprint([]byte(`{"messages":[{"role":"system","content":"<env>\n  Working directory: /work/site\n</env>"}]}`))},
		},
		{
			name:    "windows working directory",
			headers: claudeUA,
			body:    `{"system":"Primary working directory: C:\\Users\\me\\proj\nPlatform: win32"}`,
			want:    identity{AgentName: "claude", HarnessID: "claude", HarnessVersion: "2.1.0", Cwd: `C:\Users\me\proj`, HarnessSessionID: conversationFingerprint([]byte(`{"system":"Primary working directory: C:\\Users\\me\\proj\nPlatform: win32"}`))},
		},
		{
			name: "x-tapes headers override detection",
			headers: http.Header{
				"User-Agent": {"claude-cli/2.1.0"}, "X-Claude-Code-Session-Id": {"cc-1"},
				"X-Tapes-Agent-Name": {"bot"}, "X-Tapes-Harness-Id": {"mine"}, "X-Tapes-Harness-Version": {"9"},
				"X-Tapes-Harness-Session-Id": {"s1"}, "X-Tapes-Parent-Harness-Session-Id": {"p1"}, "X-Tapes-Cwd": {"/x"},
			},
			scope: laptop,
			want:  identity{AgentName: "bot", AuthSubject: "laptop", HarnessID: "mine", HarnessVersion: "9", HarnessSessionID: "s1", ParentHarnessSessionID: "p1", Cwd: "/x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := identify(tc.headers, []byte(tc.body), tc.scope, names); got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
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
