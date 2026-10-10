package usage

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func TestParseClaude(t *testing.T) {
	u, err := ParseClaude([]byte(`{
		"five_hour": {"utilization": 42.5, "resets_at": "2026-10-10T14:00:00Z"},
		"seven_day": {"utilization": 61, "resets_at": "2026-10-11T03:00:00+00:00"},
		"seven_day_opus": null,
		"extra_usage": {"is_enabled": true, "monthly_limit": 5000, "used_credits": 1250, "utilization": 25}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{
		FiveHour: &Window{UsedPercent: 42.5, ResetsAt: time.Date(2026, 10, 10, 14, 0, 0, 0, time.UTC)},
		Weekly:   &Window{UsedPercent: 61, ResetsAt: time.Date(2026, 10, 11, 3, 0, 0, 0, time.UTC)},
		Credits:  &Credits{Available: true, Detail: "$37.50 of $50.00 monthly extra usage left"},
	}
	assertUsage(t, u, want)
}

func TestParseClaudeExtraUsageSpentOrDisabled(t *testing.T) {
	spent, _ := ParseClaude([]byte(`{"extra_usage": {"is_enabled": true, "monthly_limit": 5000, "used_credits": 5000}}`))
	if spent.Credits.Available {
		t.Errorf("spent credits reported available: %+v", spent.Credits)
	}
	off, _ := ParseClaude([]byte(`{"extra_usage": {"is_enabled": false, "monthly_limit": 5000, "used_credits": 0}}`))
	if off.Credits.Available || off.Credits.Detail != "extra usage disabled" {
		t.Errorf("disabled: %+v", off.Credits)
	}
}

func TestParseCodexClassifiesWindowsByLength(t *testing.T) {
	// Weekly window listed first, numeric strings, one reset given relative to now.
	u, err := ParseCodex([]byte(`{
		"rate_limit": {
			"primary_window": {"used_percent": "88", "limit_window_seconds": 604800, "reset_at": 1760140800},
			"secondary_window": {"used_percent": 12, "limit_window_seconds": 18000, "reset_after_seconds": 3600}
		},
		"credits": {"has_credits": true, "unlimited": false, "balance": "12.5"}
	}`), now)
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{
		FiveHour: &Window{UsedPercent: 12, ResetsAt: now.Add(time.Hour)},
		Weekly:   &Window{UsedPercent: 88, ResetsAt: time.Unix(1760140800, 0).UTC()},
		Credits:  &Credits{Available: true, Detail: "credit balance 12.5"},
	}
	assertUsage(t, u, want)
}

func TestParseCodexZeroBalanceIsUnavailable(t *testing.T) {
	u, _ := ParseCodex([]byte(`{"credits": {"has_credits": true, "balance": "0"}}`), now)
	if u.Credits.Available {
		t.Errorf("zero balance reported available")
	}
}

func assertUsage(t *testing.T, got, want Usage) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("usage mismatch\n got: %s\nwant: %s", g, w)
	}
}

type fakeHost struct {
	auths    []pluginapi.HostAuthFileEntry
	creds    map[string]string
	bodies   map[string]string
	status   int
	requests []pluginapi.HTTPRequest
}

func (f *fakeHost) ListAuths() ([]pluginapi.HostAuthFileEntry, error) { return f.auths, nil }

func (f *fakeHost) AuthJSON(idx string) (json.RawMessage, error) {
	if c, ok := f.creds[idx]; ok {
		return json.RawMessage(c), nil
	}
	return nil, errors.New("unknown auth")
}

func (f *fakeHost) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.requests = append(f.requests, req)
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(f.bodies[req.URL])}, nil
}

func TestPollerFetchesOAuthAccountsAndSkipsOthers(t *testing.T) {
	host := &fakeHost{
		auths: []pluginapi.HostAuthFileEntry{
			{ID: "claude-me.json", AuthIndex: "a1", Provider: "claude", Email: "me@example.com"},
			{ID: "codex-me.json", AuthIndex: "a2", Provider: "codex"},
			{ID: "claude-off.json", AuthIndex: "a3", Provider: "claude", Disabled: true},
			{ID: "gemini.json", AuthIndex: "a4", Provider: "gemini"},
		},
		creds: map[string]string{
			"a1": `{"access_token":"tok-claude"}`,
			"a2": `{"access_token":"tok-codex","account_id":"acct-9"}`,
		},
		bodies: map[string]string{
			"http://claude/usage": `{"seven_day":{"utilization":50,"resets_at":"2026-10-11T00:00:00Z"}}`,
			"http://codex/usage":  `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`,
		},
	}
	p := NewPoller(host)
	p.Configure(Settings{Interval: time.Minute, ClaudeURL: "http://claude/usage", CodexURL: "http://codex/usage"})
	p.Refresh(now)

	accounts := p.Accounts()
	if len(accounts) != 2 || accounts[0].ID != "claude-me.json" || accounts[1].ID != "codex-me.json" {
		t.Fatalf("accounts: %+v", accounts)
	}
	if accounts[0].Label != "me@example.com" {
		t.Errorf("label: %q", accounts[0].Label)
	}
	if u, ok := p.Get("claude-me.json", now); !ok || u.Weekly.UsedPercent != 50 {
		t.Errorf("claude usage: %+v %v", u, ok)
	}
	if u, ok := p.Get("codex-me.json", now); !ok || u.FiveHour.UsedPercent != 10 {
		t.Errorf("codex usage: %+v %v", u, ok)
	}
	if got := host.requests[0].Headers.Get("Authorization"); got != "Bearer tok-claude" {
		t.Errorf("claude auth header: %q", got)
	}
	if got := host.requests[1].Headers.Get("Chatgpt-Account-Id"); got != "acct-9" {
		t.Errorf("codex account header: %q", got)
	}
}

func TestPollerKeepsLastReadingOnErrorUntilStale(t *testing.T) {
	host := &fakeHost{
		auths:  []pluginapi.HostAuthFileEntry{{ID: "c", AuthIndex: "a1", Provider: "claude"}},
		creds:  map[string]string{"a1": `{"access_token":"t"}`},
		bodies: map[string]string{ClaudeURL: `{"seven_day":{"utilization":20}}`},
	}
	p := NewPoller(host)
	p.Configure(Settings{Interval: time.Minute})
	p.Refresh(now)

	host.status = http.StatusUnauthorized
	p.Refresh(now.Add(time.Minute))
	accounts := p.Accounts()
	if accounts[0].Error != "usage endpoint returned 401" {
		t.Errorf("error not recorded: %+v", accounts[0])
	}
	if _, ok := p.Get("c", now.Add(2*time.Minute)); !ok {
		t.Errorf("last good reading dropped too early")
	}
	if _, ok := p.Get("c", now.Add(4*time.Minute)); ok {
		t.Errorf("stale reading still used for routing")
	}
}
