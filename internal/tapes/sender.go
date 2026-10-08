package tapes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/config"
)

// Logger receives the sender's diagnostics. level is one of debug, info, warn.
type Logger func(level, message string, fields map[string]any)

const workers = 2

// Sender delivers turns to tapes from a bounded queue so the proxy's request
// path never waits on tapes. When the queue is full, turns are dropped.
type Sender struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
	log      Logger

	mu     sync.RWMutex
	closed bool
	queue  chan []byte
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	// failing is set after a failed delivery and cleared by the next success,
	// so an unreachable tapes server logs one warning instead of one per turn.
	failing atomic.Bool
	sent    atomic.Int64
	failed  atomic.Int64
	dropped atomic.Int64
}

func NewSender(cfg config.Config, log Logger) *Sender {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sender{
		endpoint: cfg.IngestEndpoint,
		headers:  cfg.Headers,
		client:   &http.Client{Timeout: cfg.Timeout},
		log:      log,
		queue:    make(chan []byte, cfg.QueueSize),
		ctx:      ctx,
		cancel:   cancel,
	}
	s.wg.Add(workers)
	for range workers {
		go s.work()
	}
	return s
}

// Enqueue schedules a turn for delivery without blocking.
func (s *Sender) Enqueue(turn []byte) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- turn:
	default:
		if n := s.dropped.Add(1); n == 1 || n%100 == 0 {
			s.log("warn", "tapes: queue full, dropping turns", map[string]any{"dropped_total": n})
		}
	}
}

// Close stops accepting turns and waits for queued ones to be delivered
// until ctx ends, after which in-flight requests are cancelled.
func (s *Sender) Close(ctx context.Context) {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.cancel()
		<-done
	}
	s.cancel()
}

// Stats reports delivery counters.
func (s *Sender) Stats() (sent, failed, dropped int64) {
	return s.sent.Load(), s.failed.Load(), s.dropped.Load()
}

func (s *Sender) work() {
	defer s.wg.Done()
	for turn := range s.queue {
		if err := s.post(turn); err != nil {
			s.failed.Add(1)
			if !s.failing.Swap(true) {
				s.log("warn", "tapes: delivery failing", map[string]any{"endpoint": s.endpoint, "error": err.Error()})
			} else {
				s.log("debug", "tapes: delivery failed", map[string]any{"error": err.Error()})
			}
			continue
		}
		s.sent.Add(1)
		if s.failing.Swap(false) {
			s.log("info", "tapes: delivery recovered", map[string]any{"endpoint": s.endpoint})
		}
	}
}

func (s *Sender) post(turn []byte) error {
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, s.endpoint, bytes.NewReader(turn))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range s.headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("tapes ingest returned %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return nil
}
