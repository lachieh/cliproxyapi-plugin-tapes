package tapes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// session identifies which harness session a turn belongs to. tapes groups
// turns into sessions by (harness_id, harness_session_id).
type session struct {
	HarnessID              string
	HarnessSessionID       string
	ParentHarnessSessionID string
	// ThreadID marks a subagent's turns within the session.
	ThreadID  string
	AgentName string
}

var claudeLegacyUserID = regexp.MustCompile(`_session_([0-9a-fA-F-]{36})`)

// identifySession derives the session from what coding agents already send.
// Explicit x-tapes-* headers win, so any client can opt into precise
// grouping. Without any identifier the session falls back to a hash of the
// conversation's opening, which stays stable as later turns resend history.
func identifySession(headers http.Header, request []byte) session {
	s := session{
		HarnessID:              header(headers, "X-Tapes-Harness-Id"),
		HarnessSessionID:       header(headers, "X-Tapes-Harness-Session-Id"),
		ParentHarnessSessionID: header(headers, "X-Tapes-Parent-Harness-Session-Id"),
		ThreadID:               header(headers, "X-Claude-Code-Agent-Id"),
		AgentName:              header(headers, "X-Tapes-Agent-Name"),
	}
	if s.HarnessID == "" {
		s.HarnessID = harnessFromUserAgent(header(headers, "User-Agent"))
	}
	if s.HarnessSessionID != "" {
		return s
	}

	var body struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	_ = json.Unmarshal(request, &body)

	switch {
	case header(headers, "X-Claude-Code-Session-Id") != "":
		s.HarnessSessionID = header(headers, "X-Claude-Code-Session-Id")
		s.HarnessID = "claude"
	case claudeSessionFromUserID(body.Metadata.UserID) != "":
		s.HarnessSessionID = claudeSessionFromUserID(body.Metadata.UserID)
		s.HarnessID = "claude"
	case header(headers, "Session_id") != "" || header(headers, "Session-Id") != "":
		s.HarnessSessionID = firstNonEmpty(header(headers, "Session_id"), header(headers, "Session-Id"))
		s.HarnessID = "codex"
	case header(headers, "X-Session-Id") != "":
		s.HarnessSessionID = header(headers, "X-Session-Id")
	case body.PromptCacheKey != "":
		// Codex sets prompt_cache_key to its conversation ID.
		s.HarnessSessionID = body.PromptCacheKey
	default:
		s.HarnessSessionID = conversationFingerprint(request)
	}
	return s
}

// claudeSessionFromUserID reads the session from Claude Code's
// metadata.user_id, which is either a JSON object or the legacy
// "user_<hash>_account_<uuid>_session_<uuid>" string.
func claudeSessionFromUserID(userID string) string {
	if strings.HasPrefix(userID, "{") {
		var v struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(userID), &v) == nil {
			return v.SessionID
		}
		return ""
	}
	if m := claudeLegacyUserID.FindStringSubmatch(userID); m != nil {
		return m[1]
	}
	return ""
}

func harnessFromUserAgent(ua string) string {
	ua = strings.ToLower(ua)
	for _, known := range []struct{ marker, harness string }{
		{"claude-cli", "claude"},
		{"claude-code", "claude"},
		{"codex", "codex"},
		{"opencode", "opencode"},
		{"gemini-cli", "gemini"},
	} {
		if strings.Contains(ua, known.marker) {
			return known.harness
		}
	}
	return "cliproxyapi"
}

// conversationFingerprint hashes the system prompt and first message of a
// Chat Completions, Messages or Responses request. Cache-control markers are
// removed because clients move them between turns.
func conversationFingerprint(request []byte) string {
	var body struct {
		System       json.RawMessage   `json:"system"`
		Instructions json.RawMessage   `json:"instructions"`
		Messages     []json.RawMessage `json:"messages"`
		Input        json.RawMessage   `json:"input"`
	}
	if json.Unmarshal(request, &body) != nil {
		return ""
	}
	opening := []any{canonical(body.System), canonical(body.Instructions)}
	if len(body.Messages) > 0 {
		opening = append(opening, canonical(body.Messages[0]))
	}
	var input []json.RawMessage
	if json.Unmarshal(body.Input, &input) == nil && len(input) > 0 {
		opening = append(opening, canonical(input[0]))
	} else {
		opening = append(opening, canonical(body.Input))
	}
	// encoding/json sorts map keys, so equal content always hashes equally.
	raw, _ := json.Marshal(opening)
	sum := sha256.Sum256(raw)
	return "cpa-" + hex.EncodeToString(sum[:8])
}

func canonical(raw json.RawMessage) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return stripCacheControl(v)
}

func stripCacheControl(v any) any {
	switch t := v.(type) {
	case map[string]any:
		delete(t, "cache_control")
		for k, child := range t {
			t[k] = stripCacheControl(child)
		}
	case []any:
		for i, child := range t {
			t[i] = stripCacheControl(child)
		}
	}
	return v
}

// header looks a header up case-insensitively, because the host forwards
// client headers without guaranteeing canonical keys.
func header(h http.Header, name string) string {
	if v := strings.TrimSpace(h.Get(name)); v != "" {
		return v
	}
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
