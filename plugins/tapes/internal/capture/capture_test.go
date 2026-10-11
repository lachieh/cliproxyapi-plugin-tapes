package capture

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

func buildSSE(chunks ...string) string {
	var stream []byte
	for _, c := range chunks {
		stream = appendSSE(stream, []byte(c))
	}
	return string(terminateFrame(stream))
}

func TestAppendSSE(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name:   "openai chat bare json",
			chunks: []string{`{"id":"a"}`, `{"id":"b"}`},
			want:   "data: {\"id\":\"a\"}\n\ndata: {\"id\":\"b\"}\n\n",
		},
		{
			name:   "claude framed events",
			chunks: []string{"event: message_start\ndata: {\"a\":1}\n\n", "event: message_stop\ndata: {\"b\":2}\n\n"},
			want:   "event: message_start\ndata: {\"a\":1}\n\nevent: message_stop\ndata: {\"b\":2}\n\n",
		},
		{
			name:   "frames missing blank-line terminators",
			chunks: []string{"event: a\ndata: {\"a\":1}\n", "event: b\ndata: {\"b\":2}"},
			want:   "event: a\ndata: {\"a\":1}\n\nevent: b\ndata: {\"b\":2}\n\n",
		},
		{
			name:   "event and data split across chunks",
			chunks: []string{"event: response.created", "data: {\"x\":1}", "event: response.completed", "data: {\"y\":2}"},
			want:   "event: response.created\ndata: {\"x\":1}\n\nevent: response.completed\ndata: {\"y\":2}\n\n",
		},
		{
			name:   "consecutive data lines become separate events",
			chunks: []string{"data: {\"x\":1}\n", "data: {\"y\":2}\n"},
			want:   "data: {\"x\":1}\n\ndata: {\"y\":2}\n\n",
		},
		{
			name:   "json split mid-frame",
			chunks: []string{"data: {\"text\":\"hel", "lo\"}\n\n"},
			want:   "data: {\"text\":\"hello\"}\n\n",
		},
		{
			name:   "blank and leading-newline chunks",
			chunks: []string{"\n", "\nevent: a\ndata: {}\n", "\n", "\ndata: {}"},
			want:   "event: a\ndata: {}\n\ndata: {}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildSSE(tc.chunks...); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestStreamExchange(t *testing.T) {
	r := NewRecorder()
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", SourceFormat: "openai", Model: "gpt-x", ChunkIndex: -1, OriginalRequest: []byte(`{"stream":true}`)}, t0)
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", ChunkIndex: 0, Body: []byte(`{"n":0}`)}, t0)
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", ChunkIndex: 1, Body: []byte(`{"n":1}`)}, t0)

	x, ok := r.Complete(Completion{RequestID: "r1", Outcome: "succeeded", StatusCode: 200, StartedAt: t0, CompletedAt: t0.Add(time.Second)})
	if !ok {
		t.Fatal("expected exchange")
	}
	if !x.Stream || x.SourceFormat != "openai" || x.Model != "gpt-x" || string(x.Request) != `{"stream":true}` || x.StatusCode != 200 {
		t.Errorf("exchange = %+v", x)
	}
	if got, want := string(x.Response), "data: {\"n\":0}\n\ndata: {\"n\":1}\n\n"; got != want {
		t.Errorf("response = %q, want %q", got, want)
	}
	if r.Len() != 0 {
		t.Error("request not released")
	}
}

func TestStreamBootstrapRetryDiscardsFailedAttempt(t *testing.T) {
	r := NewRecorder()
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", ChunkIndex: -1, OriginalRequest: []byte(`{}`)}, t0)
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", ChunkIndex: 0, Body: []byte(`{"attempt":1}`)}, t0)
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", ChunkIndex: -1, OriginalRequest: []byte(`{}`)}, t0)
	r.ObserveStreamChunk(StreamChunk{RequestID: "r1", ChunkIndex: 0, Body: []byte(`{"attempt":2}`)}, t0)

	x, _ := r.Complete(Completion{RequestID: "r1", Outcome: "succeeded"})
	if got := string(x.Response); got != "data: {\"attempt\":2}\n\n" {
		t.Errorf("response = %q", got)
	}
}

func TestNonStreamExchange(t *testing.T) {
	r := NewRecorder()
	r.ObserveResponse(ResponseIntercept{RequestID: "r1", SourceFormat: "claude", OriginalRequest: []byte(`{"q":1}`), Body: []byte(`{"a":1}`), StatusCode: 200}, t0)
	x, ok := r.Complete(Completion{RequestID: "r1", Outcome: "succeeded"})
	if !ok || x.Stream || string(x.Response) != `{"a":1}` || x.StatusCode != 200 {
		t.Errorf("exchange = %+v ok=%v", x, ok)
	}
}

func TestCompleteSkips(t *testing.T) {
	cases := map[string]func(r *Recorder) Completion{
		"failed request": func(r *Recorder) Completion {
			r.ObserveResponse(ResponseIntercept{RequestID: "r", OriginalRequest: []byte(`{}`), Body: []byte(`{}`)}, t0)
			return Completion{RequestID: "r", Outcome: "failed"}
		},
		"unknown request": func(r *Recorder) Completion {
			return Completion{RequestID: "r", Outcome: "succeeded"}
		},
		"stream with no output": func(r *Recorder) Completion {
			r.ObserveStreamChunk(StreamChunk{RequestID: "r", ChunkIndex: -1, OriginalRequest: []byte(`{}`)}, t0)
			return Completion{RequestID: "r", Outcome: "canceled"}
		},
		"oversized stream": func(r *Recorder) Completion {
			r.ObserveStreamChunk(StreamChunk{RequestID: "r", ChunkIndex: -1, OriginalRequest: []byte(`{}`)}, t0)
			r.ObserveStreamChunk(StreamChunk{RequestID: "r", ChunkIndex: 0, Body: []byte(strings.Repeat("x", MaxResponseBytes))}, t0)
			r.ObserveStreamChunk(StreamChunk{RequestID: "r", ChunkIndex: 1, Body: []byte(`{}`)}, t0)
			return Completion{RequestID: "r", Outcome: "succeeded"}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			r := NewRecorder()
			if x, ok := r.Complete(setup(r)); ok {
				t.Errorf("unexpected exchange %+v", x)
			}
			if r.Len() != 0 {
				t.Error("request not released")
			}
		})
	}
}

func TestSweep(t *testing.T) {
	r := NewRecorder()
	r.ObserveResponse(ResponseIntercept{RequestID: "old-done", OriginalRequest: []byte(`{}`), Body: []byte(`{}`)}, t0)
	r.ObserveStreamChunk(StreamChunk{RequestID: "old-stream", ChunkIndex: -1, OriginalRequest: []byte(`{}`)}, t0)
	r.ObserveResponse(ResponseIntercept{RequestID: "new", OriginalRequest: []byte(`{}`), Body: []byte(`{}`)}, t0.Add(time.Hour))

	got := r.Sweep(t0.Add(time.Minute))
	if len(got) != 1 || got[0].RequestID != "old-done" {
		t.Errorf("swept = %+v", got)
	}
	if r.Len() != 1 {
		t.Errorf("remaining = %d, want 1", r.Len())
	}
}
