// Package capture assembles complete request/response exchanges from the
// separate hook calls CLIProxyAPI makes for one request.
//
// The host calls response.intercept_after for successful non-streaming
// responses, response.intercept_stream_chunk for each streamed chunk, and
// request.complete once the request ends. All three share a RequestID.
package capture

import (
	"net/http"
	"sync"
	"time"
)

// The host serializes its pluginapi structs with plain encoding/json, so the
// wire keys are the Go field names. Only the fields this plugin reads are
// declared.

// ResponseIntercept is the response.intercept_after payload.
type ResponseIntercept struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestHeaders  http.Header
	ResponseHeaders http.Header
	OriginalRequest []byte
	Body            []byte
	StatusCode      int
}

// StreamChunk is the response.intercept_stream_chunk payload.
type StreamChunk struct {
	RequestID       string
	SourceFormat    string
	Model           string
	RequestHeaders  http.Header
	OriginalRequest []byte
	Body            []byte
	ChunkIndex      int
}

// streamHeaderInitIndex marks the header-only call that precedes a stream's
// payload chunks and is the only call guaranteed to carry OriginalRequest.
const streamHeaderInitIndex = -1

// Completion is the request.complete payload.
type Completion struct {
	RequestID   string
	Outcome     string
	StatusCode  int
	StartedAt   time.Time
	CompletedAt time.Time
}

// Exchange is one finished request/response pair, in the client-facing
// protocol identified by SourceFormat.
type Exchange struct {
	RequestID      string
	SourceFormat   string
	Model          string
	Stream         bool
	RequestHeaders http.Header
	Request        []byte
	// Response is the JSON body, or for streams the SSE stream as sent to the client.
	Response    []byte
	StatusCode  int
	StartedAt   time.Time
	CompletedAt time.Time
}

// MaxResponseBytes matches the largest raw response tapes will reduce.
const MaxResponseBytes = 8 << 20

type pending struct {
	exchange  Exchange
	firstSeen time.Time
	// complete is set once a non-streaming response body has arrived.
	complete bool
	overflow bool
}

// Recorder tracks in-flight requests. It is safe for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	inFlight map[string]*pending
}

func NewRecorder() *Recorder {
	return &Recorder{inFlight: make(map[string]*pending)}
}

func (r *Recorder) ObserveResponse(ev ResponseIntercept, now time.Time) {
	if ev.RequestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight[ev.RequestID] = &pending{
		exchange: Exchange{
			RequestID:      ev.RequestID,
			SourceFormat:   ev.SourceFormat,
			Model:          ev.Model,
			RequestHeaders: ev.RequestHeaders,
			Request:        ev.OriginalRequest,
			Response:       ev.Body,
			StatusCode:     ev.StatusCode,
		},
		firstSeen: now,
		complete:  true,
		overflow:  len(ev.Body) > MaxResponseBytes,
	}
}

func (r *Recorder) ObserveStreamChunk(ev StreamChunk, now time.Time) {
	if ev.RequestID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ev.ChunkIndex == streamHeaderInitIndex {
		// A bootstrap retry repeats header-init under the same RequestID, so
		// this also discards anything captured from the failed attempt.
		r.inFlight[ev.RequestID] = &pending{
			exchange: Exchange{
				RequestID:      ev.RequestID,
				SourceFormat:   ev.SourceFormat,
				Model:          ev.Model,
				Stream:         true,
				RequestHeaders: ev.RequestHeaders,
				Request:        ev.OriginalRequest,
			},
			firstSeen: now,
		}
		return
	}
	p, ok := r.inFlight[ev.RequestID]
	if !ok || p.overflow {
		return
	}
	p.exchange.Response = appendSSE(p.exchange.Response, ev.Body)
	if len(p.exchange.Response) > MaxResponseBytes {
		p.overflow = true
		p.exchange.Response = nil
	}
}

// Complete removes the request and returns its exchange if it is worth
// recording: it succeeded (or a stream was cut short after producing output)
// and fits within MaxResponseBytes.
func (r *Recorder) Complete(ev Completion) (Exchange, bool) {
	r.mu.Lock()
	p, ok := r.inFlight[ev.RequestID]
	delete(r.inFlight, ev.RequestID)
	r.mu.Unlock()

	if !ok || p.overflow || len(p.exchange.Request) == 0 || len(p.exchange.Response) == 0 {
		return Exchange{}, false
	}
	if ev.Outcome != "succeeded" && ev.Outcome != "canceled" {
		return Exchange{}, false
	}
	x := p.exchange
	if x.Stream {
		x.Response = terminateFrame(x.Response)
	}
	if ev.StatusCode != 0 {
		x.StatusCode = ev.StatusCode
	}
	x.StartedAt = ev.StartedAt
	x.CompletedAt = ev.CompletedAt
	return x, true
}

// Sweep drops requests first seen before cutoff, which covers hosts that
// never deliver request.complete. Finished non-streaming exchanges are
// returned so they are still recorded; partial streams are discarded.
func (r *Recorder) Sweep(cutoff time.Time) []Exchange {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Exchange
	for id, p := range r.inFlight {
		if p.firstSeen.After(cutoff) {
			continue
		}
		delete(r.inFlight, id)
		if p.complete && !p.overflow && len(p.exchange.Request) > 0 && len(p.exchange.Response) > 0 {
			x := p.exchange
			x.CompletedAt = p.firstSeen
			out = append(out, x)
		}
	}
	return out
}

// Len reports how many requests are being tracked.
func (r *Recorder) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inFlight)
}
