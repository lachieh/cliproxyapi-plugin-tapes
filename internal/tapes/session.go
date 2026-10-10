package tapes

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// harnessFallback is the harness_id for clients that aren't recognized.
// tapes' own "unknown" is not usable here: over the ingest API, tapes stores
// such turns without a session and never derives them.
const harnessFallback = "cliproxyapi"

// Names configures how requests are named in tapes.
type Names struct {
	// Keys maps client API key caller scopes to names (see config.CallerScope).
	Keys map[string]string
	// FallbackAgent is the agent_name when neither the key nor the tool is known.
	FallbackAgent string
}

// identity is what is known about who sent a request, expressed in tapes'
// own fields.
type identity struct {
	// AgentName is tapes' agent_name: "<key name>/<harness>", whichever parts are known.
	AgentName string
	// AuthSubject is the name of the client API key the request used.
	AuthSubject            string
	HarnessID              string
	HarnessVersion         string
	HarnessSessionID       string
	ParentHarnessSessionID string
	// Cwd is the project directory the coding agent is working in.
	Cwd string
	// ThreadID marks a subagent's turns within the session.
	ThreadID string
}

// harnesses identifies coding agents by User-Agent, using tapes' harness ids.
var harnesses = []struct {
	marker, id string
}{
	{"claude-cli", "claude"},
	{"claude-code", "claude"},
	{"codex", "codex"},
	{"opencode", "opencode"},
	{"gemini-cli", "gemini"},
}

var (
	userAgentVersion   = regexp.MustCompile(`^[^/\s]+[/\s]v?(\d[\w.+-]*)`)
	claudeLegacyUserID = regexp.MustCompile(`_session_([0-9a-fA-F-]{36})`)
	// Coding agents state the working directory in their prompts: Claude Code
	// and OpenCode in the system prompt, Codex in an <environment_context>
	// message. These match the JSON-encoded request, so a path ends at an
	// escaped newline, a quote or a tag.
	cwdPatterns = []*regexp.Regexp{
		regexp.MustCompile(`Primary working directory: ((?:[^"\\<]|\\\\)+)`),
		regexp.MustCompile(`<cwd>((?:[^"\\<]|\\\\)+)</cwd>`),
		regexp.MustCompile(`Working directory: ((?:[^"\\<]|\\\\)+)`),
	}
)

// identify works out the request's identity from what coding agents already
// send. Explicit x-tapes-* headers, the same ones tapes' own proxy reads,
// override anything detected.
func identify(headers http.Header, request []byte, callerScope string, names Names) identity {
	id := identity{
		AuthSubject:      names.Keys[callerScope],
		ThreadID:         header(headers, "X-Claude-Code-Agent-Id"),
		HarnessSessionID: sessionID(headers, request),
		Cwd:              projectDir(request),
	}
	ua := strings.ToLower(header(headers, "User-Agent"))
	id.HarnessID = harnessFallback
	for _, h := range harnesses {
		if strings.Contains(ua, h.marker) {
			id.HarnessID = h.id
			if m := userAgentVersion.FindStringSubmatch(ua); m != nil {
				id.HarnessVersion = m[1]
			}
			break
		}
	}

	override := func(field *string, name string) {
		if v := header(headers, name); v != "" {
			*field = v
		}
	}
	override(&id.HarnessID, "X-Tapes-Harness-Id")
	override(&id.HarnessVersion, "X-Tapes-Harness-Version")
	override(&id.HarnessSessionID, "X-Tapes-Harness-Session-Id")
	override(&id.ParentHarnessSessionID, "X-Tapes-Parent-Harness-Session-Id")
	override(&id.Cwd, "X-Tapes-Cwd")

	var parts []string
	if id.AuthSubject != "" {
		parts = append(parts, id.AuthSubject)
	}
	if id.HarnessID != harnessFallback {
		parts = append(parts, id.HarnessID)
	}
	id.AgentName = strings.Join(parts, "/")
	if id.AgentName == "" {
		id.AgentName = names.FallbackAgent
	}
	override(&id.AgentName, "X-Tapes-Agent-Name")
	return id
}

// sessionID returns the session ID a coding agent sent, or else a
// fingerprint of the conversation.
func sessionID(headers http.Header, request []byte) string {
	if v := firstNonEmpty(
		header(headers, "X-Claude-Code-Session-Id"),
		header(headers, "Session_id"),
		header(headers, "Session-Id"),
		header(headers, "X-Session-Id"),
	); v != "" {
		return v
	}
	var body struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		// Codex sets prompt_cache_key to its conversation ID.
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	_ = json.Unmarshal(request, &body)
	return firstNonEmpty(claudeSessionFromUserID(body.Metadata.UserID), body.PromptCacheKey, conversationFingerprint(request))
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

func projectDir(request []byte) string {
	for _, re := range cwdPatterns {
		if m := re.FindSubmatch(request); m != nil {
			return strings.TrimSpace(strings.ReplaceAll(string(m[1]), `\\`, `\`))
		}
	}
	return ""
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
