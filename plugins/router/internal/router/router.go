package router

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/lachieh/cliproxyapi-plugins/plugins/router/internal/usage"
)

// UsageSource returns a recent usage reading for an account.
type UsageSource interface {
	Get(authID string, now time.Time) (usage.Usage, bool)
}

// Router makes routing decisions from a validated Config. It is safe for concurrent use.
type Router struct {
	loc            *time.Location
	sessionHeaders []string
	stickyTTL      time.Duration
	clients        map[string]string // sha256 hex -> name
	usageSettings  usage.Settings
	state          *State
	usage          UsageSource
	routes         []route
	tree           *node
}

// Host metadata keys that carry a session identity, most specific first.
var sessionMetadataKeys = []string{"canonical_session_id", "execution_session_id", "derived_session_id"}

type request struct {
	model, provider, client, session string
	headers                          http.Header
	now                              time.Time
}

// Step is one node visited while evaluating the tree.
type Step struct {
	Node    string `json:"node"`
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

// Route implements model.route: the first matching rule whose provider has auth wins.
func (r *Router) Route(req pluginapi.ModelRouteRequest, now time.Time) pluginapi.ModelRouteResponse {
	in := request{
		model:   req.RequestedModel,
		client:  r.observeClient(req.Headers, now),
		session: r.sessionID(req.Metadata, req.Headers),
		headers: req.Headers,
		now:     now.In(r.loc),
	}
	for _, rule := range r.routes {
		if !rule.match.matches(in) || !slices.Contains(req.AvailableProviders, rule.provider) {
			continue
		}
		result := rule.provider
		if rule.model != "" {
			result += "/" + rule.model
		}
		r.state.record(Decision{At: now, Hook: "route", Rule: rule.name, Client: in.client, Session: in.session, Model: in.model, Result: result})
		return pluginapi.ModelRouteResponse{
			Handled:     true,
			TargetKind:  pluginapi.ModelRouteTargetProvider,
			Target:      rule.provider,
			TargetModel: rule.model,
			Reason:      "rule " + rule.name,
		}
	}
	return pluginapi.ModelRouteResponse{}
}

// Pick implements scheduler.pick. The host only offers usable candidates, so accounts in
// cooldown are already excluded before the tree runs.
func (r *Router) Pick(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	r.state.observe(req.Candidates, now)
	if r.tree == nil {
		return pluginapi.SchedulerPickResponse{}
	}
	in := request{
		model:    req.Model,
		provider: req.Provider,
		client:   r.observeClient(req.Options.Headers, now),
		session:  r.sessionID(req.Options.Metadata, req.Options.Headers),
		headers:  req.Options.Headers,
		now:      now.In(r.loc),
	}
	ids := make([]string, len(req.Candidates))
	for i, c := range req.Candidates {
		ids[i] = c.ID
	}
	resp, path := r.evaluate(in, ids, strings.Join(req.Providers, ","), now, true)
	r.state.record(Decision{At: now, Hook: "pick", Rule: path[len(path)-1].Name, Client: in.client, Session: in.session, Model: in.model, Result: describePick(resp), Path: path})
	return resp
}

// SimInput is a hypothetical request for Simulate.
type SimInput struct {
	Model    string            `json:"model"`
	Provider string            `json:"provider"`
	Client   string            `json:"client"`
	Session  string            `json:"session"`
	Headers  map[string]string `json:"headers"`
	At       time.Time         `json:"at"`
	// Candidates are the account IDs offered to the tree.
	Candidates []string `json:"candidates"`
}

// SimResult is what Simulate decided and why.
type SimResult struct {
	Result     string   `json:"result"`
	Path       []Step   `json:"path"`
	Candidates []string `json:"candidates"`
}

// Simulate evaluates the tree without pinning sessions or recording a decision.
func (r *Router) Simulate(in SimInput, now time.Time) SimResult {
	at := in.At
	if at.IsZero() {
		at = now
	}
	headers := http.Header{}
	for k, v := range in.Headers {
		headers.Set(k, v)
	}
	if r.tree == nil {
		return SimResult{Result: "builtin", Candidates: in.Candidates, Path: []Step{{Node: "tree", Name: "tree", Outcome: "no tree configured"}}}
	}
	req := request{model: in.Model, provider: in.Provider, client: in.Client, session: in.Session, headers: headers, now: at.In(r.loc)}
	resp, path := r.evaluate(req, in.Candidates, in.Provider, at, false)
	return SimResult{Result: describePick(resp), Path: path, Candidates: in.Candidates}
}

func describePick(resp pluginapi.SchedulerPickResponse) string {
	switch {
	case resp.Reject:
		return "reject: " + resp.RejectReason
	case resp.DelegateBuiltin != "":
		return "delegate " + resp.DelegateBuiltin
	case resp.AuthID != "":
		return resp.AuthID
	}
	return "builtin"
}

// evaluate walks the tree. When commit is false it has no side effects.
func (r *Router) evaluate(in request, candidates []string, providers string, now time.Time, commit bool) (pluginapi.SchedulerPickResponse, []Step) {
	var path []Step
	n := r.tree
	for {
		switch n.kind {
		case kindIf:
			if n.cond.matches(in) {
				path = append(path, Step{n.id, n.name, "true"})
				n = n.then
			} else {
				path = append(path, Step{n.id, n.name, "false"})
				n = n.els
			}
		case kindPick:
			id, outcome := r.pickAccount(n, in, candidates, providers, now, commit)
			path = append(path, Step{n.id, n.name, outcome})
			if id != "" {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, path
			}
			n = n.els
		case kindDelegate:
			path = append(path, Step{n.id, n.name, "delegate " + n.delegate})
			return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: n.delegate}, path
		case kindReject:
			path = append(path, Step{n.id, n.name, "reject"})
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "auth_unavailable", RejectReason: n.rejectText}, path
		default:
			path = append(path, Step{n.id, n.name, "builtin"})
			return pluginapi.SchedulerPickResponse{}, path
		}
	}
}

func (r *Router) pickAccount(n *node, in request, candidates []string, providers string, now time.Time, commit bool) (string, string) {
	type option struct {
		id   string
		rank int
		u    usage.Usage
	}
	var options []option
	for _, id := range candidates {
		rank := 0
		if len(n.accounts) > 0 {
			rank = slices.IndexFunc(n.accounts, func(g *regexp.Regexp) bool { return g.MatchString(id) })
			if rank < 0 {
				continue
			}
		}
		u, known := r.usage.Get(id, now)
		if !n.qualifies(u, known, now) {
			continue
		}
		options = append(options, option{id, rank, u})
	}
	if len(options) == 0 {
		return "", "no account qualified"
	}

	stickyKey := ""
	if n.sticky && in.session != "" {
		stickyKey = in.session + "\x00" + providers
		if id, ok := r.state.pinned(stickyKey, now); ok && slices.ContainsFunc(options, func(o option) bool { return o.id == id }) {
			if commit {
				r.state.pin(stickyKey, id, now, r.stickyTTL)
			}
			return id, "sticky " + id
		}
	}

	slices.SortStableFunc(options, func(a, b option) int {
		switch n.order {
		case OrderSoonestReset:
			if c := compareResets(window(a.u, n.window), window(b.u, n.window)); c != 0 {
				return c
			}
		case OrderLeastUsed:
			if c := compareUsed(window(a.u, n.window), window(b.u, n.window)); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.rank, b.rank)
	})
	id := options[0].id
	if commit && stickyKey != "" {
		r.state.pin(stickyKey, id, now, r.stickyTTL)
	}
	return id, "picked " + id
}

func window(u usage.Usage, name string) *usage.Window {
	if name == "five_hour" {
		return u.FiveHour
	}
	return u.Weekly
}

// Accounts with unknown reset times sort last.
func compareResets(a, b *usage.Window) int {
	switch {
	case a == nil || a.ResetsAt.IsZero():
		if b == nil || b.ResetsAt.IsZero() {
			return 0
		}
		return 1
	case b == nil || b.ResetsAt.IsZero():
		return -1
	}
	return a.ResetsAt.Compare(b.ResetsAt)
}

// Accounts with unknown usage sort last.
func compareUsed(a, b *usage.Window) int {
	switch {
	case a == nil:
		if b == nil {
			return 0
		}
		return 1
	case b == nil:
		return -1
	}
	return cmp.Compare(a.UsedPercent, b.UsedPercent)
}

func (n *node) qualifies(u usage.Usage, known bool, now time.Time) bool {
	w := n.where
	if w.FiveHour == nil && w.Weekly == nil && !w.Credits {
		return true
	}
	if !known {
		return false
	}
	if w.Credits && (u.Credits == nil || !u.Credits.Available) {
		return false
	}
	for name, c := range map[string]*WindowCond{"weekly": w.Weekly, "five_hour": w.FiveHour} {
		if c == nil {
			continue
		}
		got := window(u, name)
		if got == nil {
			return false
		}
		if c.UsedBelow != nil && got.UsedPercent >= *c.UsedBelow {
			return false
		}
		if c.UsedAtLeast != nil && got.UsedPercent < *c.UsedAtLeast {
			return false
		}
		if d := n.within[name]; d > 0 && (got.ResetsAt.IsZero() || got.ResetsAt.Sub(now) > d) {
			return false
		}
	}
	return true
}

// observeClient resolves the client key to a configured name and records it so it can be
// named in the UI. Only a hash and the last four characters are kept.
func (r *Router) observeClient(headers http.Header, now time.Time) string {
	key := clientKey(headers)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	digest := hex.EncodeToString(sum[:])
	name := r.clients[digest]
	hint := key
	if len(hint) > 4 {
		hint = "…" + hint[len(hint)-4:]
	}
	r.state.observeClient(digest, hint, name, now)
	return name
}

func clientKey(h http.Header) string {
	if v, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	for _, name := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		if v := strings.TrimSpace(h.Get(name)); v != "" {
			return v
		}
	}
	return ""
}

func (r *Router) sessionID(meta map[string]any, headers http.Header) string {
	for _, k := range sessionMetadataKeys {
		if s, ok := meta[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	for _, h := range r.sessionHeaders {
		if v := strings.TrimSpace(headers.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

func (m matcher) matches(in request) bool {
	for _, c := range []struct {
		glob  *regexp.Regexp
		value string
	}{{m.model, in.model}, {m.provider, in.provider}, {m.client, in.client}, {m.session, in.session}} {
		if c.glob != nil && !c.glob.MatchString(c.value) {
			return false
		}
	}
	for name, glob := range m.headers {
		if !slices.ContainsFunc(in.headers.Values(name), glob.MatchString) {
			return false
		}
	}
	if m.days != nil && !m.days[in.now.Weekday()] {
		return false
	}
	if m.window != nil {
		minute := in.now.Hour()*60 + in.now.Minute()
		start, end := m.window[0], m.window[1]
		inside := minute >= start && minute < end
		if end <= start {
			inside = minute >= start || minute < end
		}
		if !inside {
			return false
		}
	}
	return true
}
