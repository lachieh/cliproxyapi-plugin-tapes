package router

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func mustParse(t *testing.T, yaml string) *Router {
	t.Helper()
	r, err := Parse([]byte(yaml), NewState())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

func candidates(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, len(ids))
	for i, id := range ids {
		out[i] = pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude"}
	}
	return out
}

func pickReq(model, session string, ids ...string) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Providers:  []string{"claude"},
		Model:      model,
		Options:    pluginapi.SchedulerOptions{Headers: http.Header{"X-Session-Id": {session}}},
		Candidates: candidates(ids...),
	}
}

// Wednesday 2026-10-07 10:00 UTC.
var weekdayMorning = time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)

func TestPickPrefersFirstAvailableAccountInOrder(t *testing.T) {
	r := mustParse(t, `
timezone: UTC
accounts:
  - match: {model: "claude-*"}
    prefer: ["personal-*", "work-*"]
`)
	got := r.Pick(pickReq("claude-opus-4", "", "work-a.json", "personal-max.json"), weekdayMorning)
	if !got.Handled || got.AuthID != "personal-max.json" {
		t.Fatalf("want personal-max.json, got %+v", got)
	}

	// Personal is in cooldown, so the host omits it; the next preference wins.
	got = r.Pick(pickReq("claude-opus-4", "", "work-a.json"), weekdayMorning)
	if got.AuthID != "work-a.json" {
		t.Fatalf("want work-a.json, got %+v", got)
	}
}

func TestPickLeavesUnmatchedRequestsToHost(t *testing.T) {
	r := mustParse(t, `
accounts:
  - match: {model: "claude-*"}
    prefer: ["personal-*"]
`)
	if got := r.Pick(pickReq("gpt-5", "", "personal-max.json"), weekdayMorning); got.Handled {
		t.Fatalf("want unhandled, got %+v", got)
	}
}

func TestStickySessionStaysOnAccountUntilItDisappears(t *testing.T) {
	r := mustParse(t, `
session_headers: [X-Session-Id]
sticky_ttl: 1h
accounts:
  - prefer: ["personal-*", "work-*"]
    sticky: true
`)
	// Personal is cooling down when the session starts, so it pins to work.
	first := r.Pick(pickReq("m", "s1", "work-a.json"), weekdayMorning)
	if first.AuthID != "work-a.json" {
		t.Fatalf("first pick: %+v", first)
	}
	// Personal recovers, but the session keeps its account.
	again := r.Pick(pickReq("m", "s1", "personal-max.json", "work-a.json"), weekdayMorning.Add(30*time.Minute))
	if again.AuthID != "work-a.json" {
		t.Fatalf("sticky pick: %+v", again)
	}
	// A different session follows preference.
	other := r.Pick(pickReq("m", "s2", "personal-max.json", "work-a.json"), weekdayMorning)
	if other.AuthID != "personal-max.json" {
		t.Fatalf("other session: %+v", other)
	}
	// Pinned account becomes unavailable: re-pick by preference and re-pin.
	moved := r.Pick(pickReq("m", "s1", "personal-max.json"), weekdayMorning.Add(40*time.Minute))
	if moved.AuthID != "personal-max.json" {
		t.Fatalf("moved pick: %+v", moved)
	}
	back := r.Pick(pickReq("m", "s1", "personal-max.json", "work-a.json"), weekdayMorning.Add(50*time.Minute))
	if back.AuthID != "personal-max.json" {
		t.Fatalf("re-pinned pick: %+v", back)
	}
}

func TestStickyPinExpiresAfterIdleTTL(t *testing.T) {
	r := mustParse(t, `
session_headers: [X-Session-Id]
sticky_ttl: 1h
accounts:
  - prefer: ["personal-*", "work-*"]
    sticky: true
`)
	r.Pick(pickReq("m", "s1", "work-a.json"), weekdayMorning)
	got := r.Pick(pickReq("m", "s1", "personal-max.json", "work-a.json"), weekdayMorning.Add(61*time.Minute))
	if got.AuthID != "personal-max.json" {
		t.Fatalf("want expired pin to follow preference, got %+v", got)
	}
}

func TestStickySessionFromHostMetadataWinsOverHeader(t *testing.T) {
	r := mustParse(t, `
session_headers: [X-Session-Id]
accounts:
  - prefer: ["a", "b"]
    sticky: true
`)
	req := pickReq("m", "header-session", "b")
	req.Options.Metadata = map[string]any{"canonical_session_id": "meta-session"}
	r.Pick(req, weekdayMorning)

	// Same metadata session, different header: still pinned to b.
	req = pickReq("m", "another-header", "a", "b")
	req.Options.Metadata = map[string]any{"canonical_session_id": "meta-session"}
	if got := r.Pick(req, weekdayMorning); got.AuthID != "b" {
		t.Fatalf("want b, got %+v", got)
	}
}

func TestFallbacks(t *testing.T) {
	for _, tc := range []struct {
		fallback string
		want     pluginapi.SchedulerPickResponse
	}{
		{"reject", pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: "rule only-personal: no preferred account is available"}},
		{"round-robin", pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: "round-robin"}},
		{"builtin", pluginapi.SchedulerPickResponse{}},
	} {
		t.Run(tc.fallback, func(t *testing.T) {
			r := mustParse(t, `
accounts:
  - name: only-personal
    prefer: ["personal-*"]
    fallback: `+tc.fallback+`
`)
			if got := r.Pick(pickReq("m", "", "work-a.json"), weekdayMorning); got != tc.want {
				t.Fatalf("want %+v, got %+v", tc.want, got)
			}
		})
	}
}

func TestMatchHeadersHoursAndDays(t *testing.T) {
	r := mustParse(t, `
timezone: Australia/Sydney
accounts:
  - name: work-hours
    match:
      headers: {User-Agent: "claude-cli/*"}
      hours: "09:00-17:00"
      days: [mon, tue, wed, thu, fri]
    prefer: ["work-*"]
  - prefer: ["personal-*"]
`)
	syd, _ := time.LoadLocation("Australia/Sydney")
	req := func(ua string) pluginapi.SchedulerPickRequest {
		q := pickReq("m", "", "personal-max.json", "work-a.json")
		q.Options.Headers = http.Header{"User-Agent": {ua}}
		return q
	}
	for _, tc := range []struct {
		name string
		ua   string
		at   time.Time
		want string
	}{
		{"weekday in hours", "claude-cli/2.1", time.Date(2026, 10, 7, 10, 0, 0, 0, syd), "work-a.json"},
		{"weekday after hours", "claude-cli/2.1", time.Date(2026, 10, 7, 17, 0, 0, 0, syd), "personal-max.json"},
		{"weekend", "claude-cli/2.1", time.Date(2026, 10, 10, 10, 0, 0, 0, syd), "personal-max.json"},
		{"other client", "curl/8", time.Date(2026, 10, 7, 10, 0, 0, 0, syd), "personal-max.json"},
		// Same instant as "weekday in hours", expressed in UTC: the rule zone still applies.
		{"utc input", "claude-cli/2.1", time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC), "work-a.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Pick(req(tc.ua), tc.at); got.AuthID != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, got)
			}
		})
	}
}

func TestHoursWindowWrapsMidnight(t *testing.T) {
	r := mustParse(t, `
timezone: UTC
accounts:
  - match: {hours: "22:00-06:00"}
    prefer: ["night"]
`)
	for _, tc := range []struct {
		hour int
		want bool
	}{{21, false}, {22, true}, {3, true}, {6, false}} {
		got := r.Pick(pickReq("m", "", "night"), time.Date(2026, 10, 7, tc.hour, 0, 0, 0, time.UTC))
		if got.Handled != tc.want {
			t.Errorf("hour %d: handled=%v, want %v", tc.hour, got.Handled, tc.want)
		}
	}
}

func TestRouteRewritesToFirstMatchingAvailableProvider(t *testing.T) {
	r := mustParse(t, `
routes:
  - name: plan-on-codex
    match: {model: "opus-plan"}
    provider: codex
    model: gpt-5.5
  - name: plan-fallback
    match: {model: "opus-plan"}
    provider: claude
    model: claude-opus-4-1
  - match: {headers: {X-Route-Profile: cheap}}
    provider: gemini
`)
	got := r.Route(pluginapi.ModelRouteRequest{RequestedModel: "opus-plan", AvailableProviders: []string{"claude", "codex"}}, weekdayMorning)
	want := pluginapi.ModelRouteResponse{Handled: true, TargetKind: "provider", Target: "codex", TargetModel: "gpt-5.5", Reason: "rule plan-on-codex"}
	if got != want {
		t.Fatalf("want %+v, got %+v", want, got)
	}

	// Codex has no auth registered: skip to the next matching rule.
	got = r.Route(pluginapi.ModelRouteRequest{RequestedModel: "opus-plan", AvailableProviders: []string{"claude"}}, weekdayMorning)
	if got.Target != "claude" || got.TargetModel != "claude-opus-4-1" {
		t.Fatalf("want claude fallback, got %+v", got)
	}

	// Header rule keeps the requested model.
	got = r.Route(pluginapi.ModelRouteRequest{
		RequestedModel:     "gemini-2.5-pro",
		Headers:            http.Header{"X-Route-Profile": {"cheap"}},
		AvailableProviders: []string{"gemini"},
	}, weekdayMorning)
	if got.Target != "gemini" || got.TargetModel != "" {
		t.Fatalf("want gemini passthrough, got %+v", got)
	}

	if got := r.Route(pluginapi.ModelRouteRequest{RequestedModel: "claude-sonnet-4", AvailableProviders: []string{"claude"}}, weekdayMorning); got.Handled {
		t.Fatalf("want unhandled, got %+v", got)
	}
}

func TestStickyPinsSurviveReconfigure(t *testing.T) {
	state := NewState()
	cfg := []byte(`
session_headers: [X-Session-Id]
accounts:
  - prefer: ["a", "b"]
    sticky: true
`)
	r1, _ := Parse(cfg, state)
	r1.Pick(pickReq("m", "s1", "b"), weekdayMorning)
	r2, _ := Parse(cfg, state)
	if got := r2.Pick(pickReq("m", "s1", "a", "b"), weekdayMorning); got.AuthID != "b" {
		t.Fatalf("want pin to survive reconfigure, got %+v", got)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ yaml, wantErr string }{
		{"acounts: []", "field acounts not found"},
		{"routes: [{match: {model: x}}]", "routes[0]: provider is required"},
		{"accounts: [{prefer: [a], fallback: nope}]", "fallback \"nope\""},
		{"accounts: [{match: {hours: 9-5}, prefer: [a]}]", "hours \"9-5\""},
		{"accounts: [{match: {days: [funday]}, prefer: [a]}]", "day \"funday\""},
		{"accounts: [{match: {model: x}}]", "rule has no effect"},
		{"sticky_ttl: -1h", "sticky_ttl"},
		{"timezone: Mars/Base", "timezone"},
	} {
		_, err := Parse([]byte(tc.yaml), NewState())
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: want error containing %q, got %v", tc.yaml, tc.wantErr, err)
		}
	}
}

func TestParseAcceptsHostFieldsAndEmptyConfig(t *testing.T) {
	for _, yaml := range []string{"", "enabled: true\npriority: 5\n"} {
		r := mustParse(t, yaml)
		if got := r.Pick(pickReq("m", "", "a"), weekdayMorning); got.Handled {
			t.Errorf("%q: empty rules should not handle, got %+v", yaml, got)
		}
	}
}

func TestSnapshotShowsSeenAccountsPinsAndDecisions(t *testing.T) {
	state := NewState()
	r, _ := Parse([]byte(`
session_headers: [X-Session-Id]
sticky_ttl: 1h
accounts:
  - name: prefer-personal
    prefer: ["personal-*"]
    sticky: true
`), state)
	r.Pick(pickReq("claude-opus-4", "s1", "work-a.json", "personal-max.json"), weekdayMorning)

	snap := state.Snapshot(weekdayMorning)
	if len(snap.Accounts) != 2 || snap.Accounts[0].ID != "personal-max.json" || snap.Accounts[1].ID != "work-a.json" {
		t.Errorf("accounts: %+v", snap.Accounts)
	}
	wantPin := Pin{Session: "s1", Providers: "claude", AuthID: "personal-max.json", Expires: weekdayMorning.Add(time.Hour)}
	if len(snap.Pins) != 1 || snap.Pins[0] != wantPin {
		t.Errorf("pins: %+v", snap.Pins)
	}
	wantDecision := Decision{At: weekdayMorning, Hook: "pick", Rule: "prefer-personal", Session: "s1", Model: "claude-opus-4", Result: "personal-max.json"}
	if len(snap.Recent) != 1 || snap.Recent[0] != wantDecision {
		t.Errorf("recent: %+v", snap.Recent)
	}
	if after := state.Snapshot(weekdayMorning.Add(2 * time.Hour)); len(after.Pins) != 0 {
		t.Errorf("expired pin still listed: %+v", after.Pins)
	}
}
