// Package tapes converts captured exchanges into tapes ingest turns and
// delivers them to a tapes ingest server (POST /v1/ingest).
package tapes

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/lachieh/cliproxyapi-plugin-tapes/internal/capture"
)

// The structs below mirror the subset of tapes' ingest contract
// (ingest.TurnPayload) this plugin fills. The reduced `response` is left out
// on purpose: tapes reduces raw_response itself, using the same reducers as
// its own capture proxy.

type turnPayload struct {
	Provider            string          `json:"provider"`
	AgentName           string          `json:"agent_name,omitempty"`
	Request             json.RawMessage `json:"request"`
	RawResponse         []byte          `json:"raw_response"`
	RawResponseEncoding string          `json:"raw_response_encoding"`
	Meta                turnMeta        `json:"meta"`
	Session             sessionEnvelope `json:"session"`
}

type turnMeta struct {
	RequestID      string  `json:"request_id"`
	ContentType    string  `json:"content_type"`
	ThreadID       string  `json:"thread_id,omitempty"`
	Model          string  `json:"model,omitempty"`
	Stream         string  `json:"stream"`
	UpstreamStatus int     `json:"upstream_status,omitempty"`
	RequestBytes   int     `json:"request_bytes"`
	ResponseBytes  int     `json:"response_bytes"`
	ElapsedSeconds float64 `json:"elapsed_seconds,omitempty"`
	TsRequest      string  `json:"ts_request,omitempty"`
	CapturedAt     string  `json:"captured_at,omitempty"`
}

type sessionEnvelope struct {
	AuthSubject            string  `json:"auth_subject"`
	HarnessID              string  `json:"harness_id"`
	HarnessSessionID       string  `json:"harness_session_id,omitempty"`
	HarnessVersion         string  `json:"harness_version,omitempty"`
	Cwd                    string  `json:"cwd,omitempty"`
	ParentHarnessSessionID *string `json:"parent_harness_session_id,omitempty"`
}

// providerFor maps CLIProxyAPI's client-facing protocol to a tapes provider.
// Captured bodies are in the client's protocol, so this is what tapes must
// parse them as, whichever upstream actually served the request.
func providerFor(sourceFormat string) (string, bool) {
	switch sourceFormat {
	case "openai", "openai-response":
		return "openai", true
	case "claude":
		return "anthropic", true
	default:
		return "", false
	}
}

// BuildTurn converts an exchange into a tapes ingest request body. keyNames
// maps client API key caller scopes to names (see config.CallerScope).
// It returns an error for exchanges tapes cannot parse.
func BuildTurn(x capture.Exchange, keyNames map[string]string) ([]byte, error) {
	provider, ok := providerFor(x.SourceFormat)
	if !ok {
		return nil, fmt.Errorf("unsupported client protocol %q (tapes accepts openai and anthropic)", x.SourceFormat)
	}
	if !json.Valid(x.Request) {
		return nil, fmt.Errorf("request body is not JSON")
	}

	id := identify(x.RequestHeaders, x.Request, x.CallerScope, keyNames)
	contentType := "application/json"
	if x.Stream {
		contentType = "text/event-stream"
	}

	turn := turnPayload{
		Provider:            provider,
		AgentName:           id.AgentName,
		Request:             x.Request,
		RawResponse:         x.Response,
		RawResponseEncoding: "identity",
		Meta: turnMeta{
			// request_id is tapes' dedupe key, so retried sends are idempotent.
			RequestID:      "cliproxyapi-" + x.RequestID,
			ContentType:    contentType,
			ThreadID:       id.ThreadID,
			Model:          x.Model,
			Stream:         strconv.FormatBool(x.Stream),
			UpstreamStatus: x.StatusCode,
			RequestBytes:   len(x.Request),
			ResponseBytes:  len(x.Response),
			TsRequest:      rfc3339(x.StartedAt),
			CapturedAt:     rfc3339(x.CompletedAt),
		},
		Session: sessionEnvelope{
			AuthSubject:      id.AuthSubject,
			HarnessID:        id.HarnessID,
			HarnessSessionID: id.HarnessSessionID,
			HarnessVersion:   id.HarnessVersion,
			Cwd:              id.Cwd,
		},
	}
	if !x.StartedAt.IsZero() && x.CompletedAt.After(x.StartedAt) {
		turn.Meta.ElapsedSeconds = x.CompletedAt.Sub(x.StartedAt).Seconds()
	}
	if id.ParentHarnessSessionID != "" {
		turn.Session.ParentHarnessSessionID = &id.ParentHarnessSessionID
	}
	return json.Marshal(turn)
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
