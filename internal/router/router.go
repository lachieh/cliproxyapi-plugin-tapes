package router

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Router makes routing decisions from a validated Config. It is safe for concurrent use.
type Router struct {
	loc            *time.Location
	sessionHeaders []string
	stickyTTL      time.Duration
	state          *State
	routes         []route
	accounts       []account
}

// Host metadata keys that carry a session identity, most specific first.
var sessionMetadataKeys = []string{"canonical_session_id", "execution_session_id", "derived_session_id"}

type request struct {
	model, provider, session string
	headers                  http.Header
	now                      time.Time
}

// Route implements model.route: the first matching rule whose provider has auth wins.
func (r *Router) Route(req pluginapi.ModelRouteRequest, now time.Time) pluginapi.ModelRouteResponse {
	in := request{
		model:   req.RequestedModel,
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
		r.state.record(Decision{At: now, Hook: "route", Rule: rule.name, Session: in.session, Model: in.model, Result: result})
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

// Pick implements scheduler.pick. The host only offers available candidates, so accounts in
// cooldown or out of quota are already excluded and preference order falls through naturally.
func (r *Router) Pick(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	r.state.observe(req.Candidates, now)
	resp, in, rule := r.pick(req, now)
	if rule != "" {
		r.state.record(Decision{At: now, Hook: "pick", Rule: rule, Session: in.session, Model: in.model, Result: describePick(resp)})
	}
	return resp
}

func describePick(resp pluginapi.SchedulerPickResponse) string {
	switch {
	case resp.Reject:
		return "reject"
	case resp.DelegateBuiltin != "":
		return "delegate " + resp.DelegateBuiltin
	case resp.AuthID != "":
		return resp.AuthID
	}
	return "builtin"
}

func (r *Router) pick(req pluginapi.SchedulerPickRequest, now time.Time) (pluginapi.SchedulerPickResponse, request, string) {
	in := request{
		model:    req.Model,
		provider: req.Provider,
		session:  r.sessionID(req.Options.Metadata, req.Options.Headers),
		headers:  req.Options.Headers,
		now:      now.In(r.loc),
	}
	for _, rule := range r.accounts {
		if !rule.match.matches(in) {
			continue
		}
		stickyKey := ""
		if rule.sticky && in.session != "" {
			stickyKey = in.session + "\x00" + strings.Join(req.Providers, ",")
			if id, ok := r.state.pinned(stickyKey, now); ok && hasCandidate(req.Candidates, id) {
				r.state.pin(stickyKey, id, now, r.stickyTTL)
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, in, rule.name
			}
		}
		if id := preferred(rule.prefer, req.Candidates); id != "" {
			if stickyKey != "" {
				r.state.pin(stickyKey, id, now, r.stickyTTL)
			}
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}, in, rule.name
		}
		return r.fallback(rule, req.Candidates, stickyKey, now), in, rule.name
	}
	return pluginapi.SchedulerPickResponse{}, in, ""
}

func (r *Router) fallback(rule account, candidates []pluginapi.SchedulerAuthCandidate, stickyKey string, now time.Time) pluginapi.SchedulerPickResponse {
	switch rule.fallback {
	case FallbackReject:
		return pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectCode:   "auth_unavailable",
			RejectReason: fmt.Sprintf("rule %s: no preferred account is available", rule.name),
		}
	case FallbackRoundRobin, FallbackFillFirst:
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: rule.fallback}
	}
	// A sticky rule with no preference still needs a first pick to pin; take the host's first candidate.
	if stickyKey != "" && len(candidates) > 0 {
		id := candidates[0].ID
		r.state.pin(stickyKey, id, now, r.stickyTTL)
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: id}
	}
	return pluginapi.SchedulerPickResponse{}
}

func preferred(prefer []*regexp.Regexp, candidates []pluginapi.SchedulerAuthCandidate) string {
	for _, p := range prefer {
		for _, c := range candidates {
			if p.MatchString(c.ID) {
				return c.ID
			}
		}
	}
	return ""
}

func hasCandidate(candidates []pluginapi.SchedulerAuthCandidate, id string) bool {
	return slices.ContainsFunc(candidates, func(c pluginapi.SchedulerAuthCandidate) bool { return c.ID == id })
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
	if m.model != nil && !m.model.MatchString(in.model) {
		return false
	}
	if m.provider != nil && !m.provider.MatchString(in.provider) {
		return false
	}
	if m.session != nil && !m.session.MatchString(in.session) {
		return false
	}
	for name, glob := range m.headers {
		values := in.headers.Values(name)
		if !slices.ContainsFunc(values, glob.MatchString) {
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
