package router

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// State is process-lifetime memory shared across config reloads: session pins, the accounts
// the host has offered (their IDs are what prefer globs match), and recent decisions.
type State struct {
	mu     sync.Mutex
	pins   map[string]pin
	seen   map[string]SeenAccount
	recent []Decision
}

type pin struct {
	authID  string
	expires time.Time
}

// SeenAccount is an account the host offered as a scheduler candidate.
type SeenAccount struct {
	ID       string    `json:"id"`
	Provider string    `json:"provider"`
	Status   string    `json:"status"`
	LastSeen time.Time `json:"last_seen"`
}

// Decision is one handled route or pick.
type Decision struct {
	At      time.Time `json:"at"`
	Hook    string    `json:"hook"`
	Rule    string    `json:"rule"`
	Session string    `json:"session,omitempty"`
	Model   string    `json:"model"`
	Result  string    `json:"result"`
}

// Pin is a session currently bound to an account.
type Pin struct {
	Session   string    `json:"session"`
	Providers string    `json:"providers"`
	AuthID    string    `json:"auth_id"`
	Expires   time.Time `json:"expires"`
}

// Snapshot is the debugging view served by the management route.
type Snapshot struct {
	Accounts []SeenAccount `json:"accounts"`
	Pins     []Pin         `json:"pins"`
	Recent   []Decision    `json:"recent"`
}

const (
	pinPruneThreshold = 4096
	recentLimit       = 50
)

// NewState returns empty state.
func NewState() *State {
	return &State{pins: make(map[string]pin), seen: make(map[string]SeenAccount)}
}

func (s *State) pinned(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pins[key]
	if !ok || !now.Before(p.expires) {
		return "", false
	}
	return p.authID, true
}

func (s *State) pin(key, authID string, now time.Time, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pins) >= pinPruneThreshold {
		s.pruneLocked(now)
	}
	s.pins[key] = pin{authID: authID, expires: now.Add(ttl)}
}

func (s *State) pruneLocked(now time.Time) {
	for k, p := range s.pins {
		if !now.Before(p.expires) {
			delete(s.pins, k)
		}
	}
}

func (s *State) observe(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range candidates {
		s.seen[c.ID] = SeenAccount{ID: c.ID, Provider: c.Provider, Status: c.Status, LastSeen: now}
	}
}

func (s *State) record(d Decision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.recent) == recentLimit {
		s.recent = s.recent[1:]
	}
	s.recent = append(s.recent, d)
}

// Snapshot returns a copy of the state, newest decisions first, with expired pins dropped.
func (s *State) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	out := Snapshot{Accounts: []SeenAccount{}, Pins: []Pin{}, Recent: []Decision{}}
	for _, a := range s.seen {
		out.Accounts = append(out.Accounts, a)
	}
	slices.SortFunc(out.Accounts, func(a, b SeenAccount) int { return strings.Compare(a.ID, b.ID) })
	for k, p := range s.pins {
		session, providers, _ := strings.Cut(k, "\x00")
		out.Pins = append(out.Pins, Pin{Session: session, Providers: providers, AuthID: p.authID, Expires: p.expires})
	}
	slices.SortFunc(out.Pins, func(a, b Pin) int { return strings.Compare(a.Session, b.Session) })
	for i := len(s.recent) - 1; i >= 0; i-- {
		out.Recent = append(out.Recent, s.recent[i])
	}
	return out
}
