package router

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/lachieh/cliproxyapi-plugins/plugins/router/internal/usage"
)

// Wednesday 2026-10-07 10:00 UTC.
var weekdayMorning = time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)

type fakeUsage map[string]usage.Usage

func (f fakeUsage) Get(id string, _ time.Time) (usage.Usage, bool) {
	u, ok := f[id]
	return u, ok
}

func mustParse(t *testing.T, yaml string, u fakeUsage) *Router {
	t.Helper()
	r, err := Parse([]byte(yaml), NewState(), u)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return r
}

func pickReq(model, session string, ids ...string) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, len(ids))
	for i, id := range ids {
		candidates[i] = pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude"}
	}
	return pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Providers:  []string{"claude"},
		Model:      model,
		Options:    pluginapi.SchedulerOptions{Headers: http.Header{"X-Session-Id": {session}}},
		Candidates: candidates,
	}
}

func weekly(used float64, resetsIn time.Duration) usage.Usage {
	return usage.Usage{Weekly: &usage.Window{UsedPercent: used, ResetsAt: weekdayMorning.Add(resetsIn)}}
}

// The motivating policy: spend allowance that is about to reset before anything else, then
// prefer the personal account, then paid credit, then give up.
const spendExpiringFirst = `
tree:
  name: expiring allowance
  pick:
    where: {weekly: {resets_within: 24h, used_below: 85}}
    order: soonest_reset
  else:
    name: personal
    pick:
      accounts: ["personal-*"]
      where: {five_hour: {used_below: 90}}
    else:
      name: paid credit
      pick: {where: {credits: true}}
      else: {name: exhausted, reject: "all accounts exhausted"}
`

func TestTreeSpendsAllowanceThatIsAboutToReset(t *testing.T) {
	u := fakeUsage{
		"personal-max.json": weekly(40, 5*24*time.Hour),
		"work-a.json":       weekly(30, 20*time.Hour),
		"work-b.json":       weekly(50, 6*time.Hour),
		"work-c.json":       weekly(90, 2*time.Hour), // resets soon but nearly used up
	}
	r := mustParse(t, spendExpiringFirst, u)
	got := r.Pick(pickReq("claude-opus-4", "", "personal-max.json", "work-a.json", "work-b.json", "work-c.json"), weekdayMorning)
	if got.AuthID != "work-b.json" {
		t.Fatalf("want work-b.json (soonest qualifying reset), got %+v", got)
	}
}

func TestTreeFallsThroughToPreferenceThenCreditThenReject(t *testing.T) {
	u := fakeUsage{
		"personal-max.json": {FiveHour: &usage.Window{UsedPercent: 50}, Weekly: &usage.Window{UsedPercent: 40, ResetsAt: weekdayMorning.Add(96 * time.Hour)}},
		"work-a.json":       {Weekly: &usage.Window{UsedPercent: 95, ResetsAt: weekdayMorning.Add(96 * time.Hour)}, Credits: &usage.Credits{Available: true}},
	}
	r := mustParse(t, spendExpiringFirst, u)

	if got := r.Pick(pickReq("m", "", "personal-max.json", "work-a.json"), weekdayMorning); got.AuthID != "personal-max.json" {
		t.Fatalf("want personal, got %+v", got)
	}

	u["personal-max.json"].FiveHour.UsedPercent = 95
	if got := r.Pick(pickReq("m", "", "personal-max.json", "work-a.json"), weekdayMorning); got.AuthID != "work-a.json" {
		t.Fatalf("want paid credit on work-a, got %+v", got)
	}

	u["work-a.json"].Credits.Available = false
	got := r.Pick(pickReq("m", "", "personal-max.json", "work-a.json"), weekdayMorning)
	want := pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: "all accounts exhausted"}
	if got != want {
		t.Fatalf("want reject, got %+v", got)
	}
}

func TestUsageConditionsExcludeAccountsWithoutReadings(t *testing.T) {
	r := mustParse(t, `
tree:
  pick: {where: {weekly: {used_below: 50}}}
`, fakeUsage{"known.json": weekly(10, time.Hour)})
	if got := r.Pick(pickReq("m", "", "unknown.json", "known.json"), weekdayMorning); got.AuthID != "known.json" {
		t.Fatalf("want known.json, got %+v", got)
	}
}

func TestLeastUsedOrderWithListedTieBreak(t *testing.T) {
	r := mustParse(t, `
tree:
  pick: {accounts: ["b*", "a*"], order: least_used, window: weekly}
`, fakeUsage{"a.json": weekly(20, time.Hour), "b.json": weekly(20, time.Hour), "c.json": weekly(1, time.Hour)})
	if got := r.Pick(pickReq("m", "", "a.json", "b.json", "c.json"), weekdayMorning); got.AuthID != "b.json" {
		t.Fatalf("want b.json (c excluded by accounts, tie broken by list order), got %+v", got)
	}
}

func TestIfBranchesOnRequestConditions(t *testing.T) {
	r := mustParse(t, `
timezone: Australia/Sydney
tree:
  name: work hours
  if:
    headers: {User-Agent: "claude-cli/*"}
    hours: "09:00-17:00"
    days: [mon, tue, wed, thu, fri]
  then: {pick: {accounts: ["work-*"]}}
  else: {pick: {accounts: ["personal-*"]}}
`, nil)
	syd, _ := time.LoadLocation("Australia/Sydney")
	for _, tc := range []struct {
		name, ua string
		at       time.Time
		want     string
	}{
		{"weekday in hours", "claude-cli/2.1", time.Date(2026, 10, 7, 10, 0, 0, 0, syd), "work-a.json"},
		{"weekday after hours", "claude-cli/2.1", time.Date(2026, 10, 7, 17, 0, 0, 0, syd), "personal-max.json"},
		{"weekend", "claude-cli/2.1", time.Date(2026, 10, 10, 10, 0, 0, 0, syd), "personal-max.json"},
		{"other client", "curl/8", time.Date(2026, 10, 7, 10, 0, 0, 0, syd), "personal-max.json"},
		{"utc input", "claude-cli/2.1", time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC), "work-a.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := pickReq("m", "", "personal-max.json", "work-a.json")
			q.Options.Headers = http.Header{"User-Agent": {tc.ua}}
			if got := r.Pick(q, tc.at); got.AuthID != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, got)
			}
		})
	}
}

func TestHoursWindowWrapsMidnight(t *testing.T) {
	r := mustParse(t, `
timezone: UTC
tree:
  if: {hours: "22:00-06:00"}
  then: {pick: {accounts: [night]}}
`, nil)
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

func sha(key string) string {
	s := sha256.Sum256([]byte(key))
	return hex.EncodeToString(s[:])
}

func TestClientConditionMatchesNamedKeyHash(t *testing.T) {
	r := mustParse(t, `
clients:
  work-laptop: `+sha("sk-work-123456")+`
tree:
  if: {client: work-laptop}
  then: {pick: {accounts: ["work-*"]}}
  else: {pick: {accounts: ["personal-*"]}}
`, nil)
	for key, want := range map[string]string{"sk-work-123456": "work-a.json", "sk-other": "personal-max.json"} {
		q := pickReq("m", "", "personal-max.json", "work-a.json")
		q.Options.Headers = http.Header{"Authorization": {"Bearer " + key}}
		if got := r.Pick(q, weekdayMorning); got.AuthID != want {
			t.Errorf("key %s: want %s, got %+v", key, want, got)
		}
	}
	clients := r.state.Snapshot(weekdayMorning).Clients
	if len(clients) != 2 {
		t.Fatalf("clients: %+v", clients)
	}
	for _, c := range clients {
		if c.SHA256 == sha("sk-work-123456") && (c.Name != "work-laptop" || c.Hint != "…3456") {
			t.Errorf("named client: %+v", c)
		}
		if strings.Contains(c.Hint, "sk-") {
			t.Errorf("hint leaks key: %+v", c)
		}
	}
}

func TestStickyHoldsUntilPinnedAccountStopsQualifying(t *testing.T) {
	u := fakeUsage{"a.json": weekly(10, 72*time.Hour), "b.json": weekly(10, 72*time.Hour)}
	r := mustParse(t, `
session_headers: [X-Session-Id]
tree:
  pick: {accounts: [a*, b*], where: {weekly: {used_below: 80}}}
  sticky: true
`, u)
	// a is unavailable when the session starts, so it pins to b.
	if got := r.Pick(pickReq("m", "s1", "b.json"), weekdayMorning); got.AuthID != "b.json" {
		t.Fatalf("first: %+v", got)
	}
	if got := r.Pick(pickReq("m", "s1", "a.json", "b.json"), weekdayMorning); got.AuthID != "b.json" {
		t.Fatalf("sticky: %+v", got)
	}
	// b crosses the usage threshold: the pin no longer qualifies and the session moves.
	u["b.json"] = weekly(85, 72*time.Hour)
	if got := r.Pick(pickReq("m", "s1", "a.json", "b.json"), weekdayMorning); got.AuthID != "a.json" {
		t.Fatalf("moved: %+v", got)
	}
}

func TestStickyPinsSurviveReconfigure(t *testing.T) {
	state := NewState()
	cfg := []byte(`
session_headers: [X-Session-Id]
tree: {pick: {accounts: [a, b]}, sticky: true}
`)
	r1, _ := Parse(cfg, state, fakeUsage{})
	r1.Pick(pickReq("m", "s1", "b"), weekdayMorning)
	r2, _ := Parse(cfg, state, fakeUsage{})
	if got := r2.Pick(pickReq("m", "s1", "a", "b"), weekdayMorning); got.AuthID != "b" {
		t.Fatalf("want pin to survive reconfigure, got %+v", got)
	}
}

func TestSimulateTracesPathWithoutSideEffects(t *testing.T) {
	u := fakeUsage{"personal-max.json": {FiveHour: &usage.Window{UsedPercent: 10}}}
	r := mustParse(t, "session_headers: [X-Session-Id]\n"+spendExpiringFirst, u)
	res := r.Simulate(SimInput{Model: "m", Provider: "claude", Session: "s9", Candidates: []string{"personal-max.json", "work-a.json"}}, weekdayMorning)
	if res.Result != "personal-max.json" {
		t.Fatalf("result: %+v", res)
	}
	want := []Step{
		{"tree", "expiring allowance", "no account qualified"},
		{"tree.else", "personal", "picked personal-max.json"},
	}
	if len(res.Path) != len(want) || res.Path[0] != want[0] || res.Path[1] != want[1] {
		t.Fatalf("path: %+v", res.Path)
	}
	if snap := r.state.Snapshot(weekdayMorning); len(snap.Recent) != 0 || len(snap.Pins) != 0 {
		t.Fatalf("simulate had side effects: %+v", snap)
	}
}

func TestPickRecordsDecisionWithPath(t *testing.T) {
	r := mustParse(t, `tree: {name: only, pick: {accounts: [a]}}`, nil)
	r.Pick(pickReq("m", "", "a"), weekdayMorning)
	recent := r.state.Snapshot(weekdayMorning).Recent
	if len(recent) != 1 || recent[0].Result != "a" || recent[0].Rule != "only" || recent[0].Path[0].Node != "tree" {
		t.Fatalf("recent: %+v", recent)
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
`, nil)
	got := r.Route(pluginapi.ModelRouteRequest{RequestedModel: "opus-plan", AvailableProviders: []string{"claude", "codex"}}, weekdayMorning)
	want := pluginapi.ModelRouteResponse{Handled: true, TargetKind: "provider", Target: "codex", TargetModel: "gpt-5.5", Reason: "rule plan-on-codex"}
	if got != want {
		t.Fatalf("want %+v, got %+v", want, got)
	}
	got = r.Route(pluginapi.ModelRouteRequest{RequestedModel: "opus-plan", AvailableProviders: []string{"claude"}}, weekdayMorning)
	if got.Target != "claude" || got.TargetModel != "claude-opus-4-1" {
		t.Fatalf("want claude fallback, got %+v", got)
	}
	if got := r.Route(pluginapi.ModelRouteRequest{RequestedModel: "claude-sonnet-4", AvailableProviders: []string{"claude"}}, weekdayMorning); got.Handled {
		t.Fatalf("want unhandled, got %+v", got)
	}
}

func TestNoTreeLeavesPicksToHost(t *testing.T) {
	// A store install adds a store manifest block next to enabled.
	storeInstalled := "enabled: true\nstore:\n  id: lachieh-router\n  version: 0.2.0\n  release-tag: v0.2.0\n  repository: lachieh/cliproxyapi-plugin-lachieh\n"
	for _, yaml := range []string{"", "enabled: true\npriority: 5\n", storeInstalled} {
		r := mustParse(t, yaml, nil)
		if got := r.Pick(pickReq("m", "", "a"), weekdayMorning); got.Handled {
			t.Errorf("%q: got %+v", yaml, got)
		}
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	for _, tc := range []struct{ yaml, wantErr string }{
		{"acounts: []", "field acounts not found"},
		{"routes: [{match: {model: x}}]", "routes[0]: provider is required"},
		{"tree: {}", "tree: set exactly one of"},
		{"tree: {pick: {}, reject: no}", "tree: set exactly one of"},
		{"tree: {if: {model: x}, then: {pick: {order: random}}}", "tree.then: order \"random\""},
		{"tree: {pick: {window: monthly}}", "window \"monthly\""},
		{"tree: {pick: {where: {weekly: {resets_within: soon}}}}", "resets_within \"soon\""},
		{"tree: {delegate: random}", "delegate \"random\""},
		{"tree: {reject: no, sticky: true}", "sticky only applies to pick"},
		{"tree: {pick: {}, then: {builtin: true}}", "then only applies to if"},
		{"tree: {if: {hours: 9-5}, then: {builtin: true}}", "hours \"9-5\""},
		{"tree: {if: {days: [funday]}, then: {builtin: true}}", "day \"funday\""},
		{"clients: {laptop: sk-raw-key}", "clients.laptop: must be the hex SHA-256"},
		{"usage: {interval: 10s}", "usage.interval"},
		{"sticky_ttl: -1h", "sticky_ttl"},
		{"timezone: Mars/Base", "timezone"},
	} {
		_, err := Parse([]byte(tc.yaml), NewState(), fakeUsage{})
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: want error containing %q, got %v", tc.yaml, tc.wantErr, err)
		}
	}
}
