package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Host is the subset of host callbacks the poller needs.
type Host interface {
	ListAuths() ([]pluginapi.HostAuthFileEntry, error)
	AuthJSON(authIndex string) (json.RawMessage, error)
	Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// Settings controls polling. Zero values select defaults.
type Settings struct {
	Interval  time.Duration
	ClaudeURL string
	CodexURL  string
}

const DefaultInterval = 5 * time.Minute

// Account is the latest usage observation for one credential.
type Account struct {
	ID        string    `json:"id"`
	AuthIndex string    `json:"auth_index"`
	Provider  string    `json:"provider"`
	Label     string    `json:"label,omitempty"`
	Usage     *Usage    `json:"usage,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// Poller periodically fetches usage for Claude and Codex OAuth accounts. Routing reads the
// cache only, so a slow or failing usage endpoint never delays a request.
type Poller struct {
	host Host

	mu       sync.Mutex
	settings Settings
	accounts map[string]Account
	kick     chan struct{}
}

func NewPoller(host Host) *Poller {
	return &Poller{host: host, accounts: make(map[string]Account), kick: make(chan struct{}, 1)}
}

// Configure replaces the settings and triggers a refresh.
func (p *Poller) Configure(s Settings) {
	if s.Interval <= 0 {
		s.Interval = DefaultInterval
	}
	if s.ClaudeURL == "" {
		s.ClaudeURL = ClaudeURL
	}
	if s.CodexURL == "" {
		s.CodexURL = CodexURL
	}
	p.mu.Lock()
	p.settings = s
	p.mu.Unlock()
	p.Kick()
}

// Kick requests a refresh without waiting for it.
func (p *Poller) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Run refreshes on every interval or kick until ctx is done.
func (p *Poller) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-p.kick:
		}
		p.Refresh(time.Now())
		timer.Reset(p.current().Interval)
	}
}

func (p *Poller) current() Settings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings
}

// Refresh fetches usage for every enabled Claude and Codex credential.
func (p *Poller) Refresh(now time.Time) {
	settings := p.current()
	if settings.Interval == 0 {
		return // not configured yet
	}
	entries, err := p.host.ListAuths()
	if err != nil {
		return
	}
	next := make(map[string]Account, len(entries))
	for _, e := range entries {
		provider := strings.ToLower(e.Provider)
		if e.Disabled || (provider != "claude" && provider != "codex") {
			continue
		}
		label := e.Email
		if label == "" {
			label = e.Label
		}
		a := Account{ID: e.ID, AuthIndex: e.AuthIndex, Provider: provider, Label: label}
		u, err := p.fetch(settings, provider, e.AuthIndex, now)
		if err != nil {
			// Keep the last good reading; staleness is handled in Get.
			prev := p.get(e.ID)
			a.Usage, a.FetchedAt, a.Error = prev.Usage, prev.FetchedAt, err.Error()
		} else {
			a.Usage, a.FetchedAt = &u, now
		}
		next[e.ID] = a
	}
	p.mu.Lock()
	p.accounts = next
	p.mu.Unlock()
}

func (p *Poller) get(id string) Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accounts[id]
}

func (p *Poller) fetch(s Settings, provider, authIndex string, now time.Time) (Usage, error) {
	raw, err := p.host.AuthJSON(authIndex)
	if err != nil {
		return Usage{}, err
	}
	var cred struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil || cred.AccessToken == "" {
		return Usage{}, fmt.Errorf("credential has no access_token")
	}
	headers := http.Header{"Authorization": {"Bearer " + cred.AccessToken}, "Accept": {"application/json"}}
	url := s.ClaudeURL
	if provider == "claude" {
		headers.Set("anthropic-beta", "oauth-2025-04-20")
		headers.Set("User-Agent", "claude-cli/2.1.280 (external, cli)")
	} else {
		url = s.CodexURL
		headers.Set("User-Agent", "codex-tui/0.149.1")
		if cred.AccountID != "" {
			headers.Set("Chatgpt-Account-Id", cred.AccountID)
		}
	}
	resp, err := p.host.Do(pluginapi.HTTPRequest{Method: http.MethodGet, URL: url, Headers: headers})
	if err != nil {
		return Usage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Usage{}, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}
	if provider == "claude" {
		return ParseClaude(resp.Body)
	}
	return ParseCodex(resp.Body, now)
}

// Get returns usage for an account if a reading is recent enough to route on: no older than
// three poll intervals, so a broken endpoint stops influencing decisions.
func (p *Poller) Get(id string, now time.Time) (Usage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[id]
	if !ok || a.Usage == nil || now.Sub(a.FetchedAt) > 3*p.settings.Interval {
		return Usage{}, false
	}
	return *a.Usage, true
}

// Accounts returns every observed account sorted by ID.
func (p *Poller) Accounts() []Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Account) int { return strings.Compare(a.ID, b.ID) })
	return out
}
