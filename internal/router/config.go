// Package router holds the routing rules and decisions, free of the C ABI.
package router

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lachieh/cliproxyapi-plugin-lachieh/internal/usage"
)

// Config is the YAML under plugins.configs.<plugin-id>.
type Config struct {
	// Enabled and Priority are interpreted by the host; declared so strict decoding accepts them.
	Enabled  *bool `yaml:"enabled"`
	Priority int   `yaml:"priority"`

	// Timezone for hours/days matching. Defaults to the host's local zone.
	Timezone string `yaml:"timezone"`
	// SessionHeaders are checked in order after host-provided session metadata.
	SessionHeaders []string `yaml:"session_headers"`
	// StickyTTL is how long a session stays pinned to an account after its last request.
	StickyTTL string `yaml:"sticky_ttl"`
	// Clients names client API keys by the hex SHA-256 of the key, so no key is stored here.
	Clients map[string]string `yaml:"clients"`
	Usage   UsageConfig       `yaml:"usage"`

	Routes []RouteRule `yaml:"routes"`
	// Tree decides which account serves a request.
	Tree *Node `yaml:"tree"`
}

// UsageConfig controls usage polling for Claude and Codex OAuth accounts.
type UsageConfig struct {
	Interval string `yaml:"interval"`
	// ClaudeURL and CodexURL override the usage endpoints, e.g. for a test server.
	ClaudeURL string `yaml:"claude_url"`
	CodexURL  string `yaml:"codex_url"`
}

// Cond is a request condition. Fields are ANDed; an empty field always matches.
type Cond struct {
	Model    string            `yaml:"model"`
	Provider string            `yaml:"provider"`
	Client   string            `yaml:"client"`
	Headers  map[string]string `yaml:"headers"`
	Session  string            `yaml:"session"`
	Hours    string            `yaml:"hours"`
	Days     []string          `yaml:"days"`
}

// RouteRule sends matching requests to a provider before account selection.
type RouteRule struct {
	Name     string `yaml:"name"`
	Match    Cond   `yaml:"match"`
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// Node is one decision tree node. Exactly one of If, Pick, Delegate, Reject, or Builtin is set.
//
//   - If: evaluate the condition, continue at Then or Else.
//   - Pick: choose an account; continue at Else when none qualifies.
//   - Delegate: hand the choice to a built-in scheduler (round-robin or fill-first).
//   - Reject: fail the request with this message.
//   - Builtin: leave the choice to the host's configured strategy.
//
// A missing Then or Else behaves like Builtin.
type Node struct {
	Name     string `yaml:"name,omitempty"`
	If       *Cond  `yaml:"if,omitempty"`
	Then     *Node  `yaml:"then,omitempty"`
	Else     *Node  `yaml:"else,omitempty"`
	Pick     *Pick  `yaml:"pick,omitempty"`
	Sticky   bool   `yaml:"sticky,omitempty"`
	Delegate string `yaml:"delegate,omitempty"`
	Reject   string `yaml:"reject,omitempty"`
	Builtin  bool   `yaml:"builtin,omitempty"`
}

// Pick selects among offered accounts that match Accounts and Where.
type Pick struct {
	// Accounts are ID globs in preference order. Empty means every offered account.
	Accounts []string `yaml:"accounts"`
	Where    *Where   `yaml:"where"`
	// Order is listed (default), soonest_reset, or least_used.
	Order string `yaml:"order"`
	// Window is the usage window Order compares: weekly (default) or five_hour.
	Window string `yaml:"window"`
}

// Where filters accounts by cached usage. An account without a recent usage reading fails any
// usage condition.
type Where struct {
	FiveHour *WindowCond `yaml:"five_hour"`
	Weekly   *WindowCond `yaml:"weekly"`
	// Credits requires paid extra usage to be available.
	Credits bool `yaml:"credits"`
}

// WindowCond constrains one usage window.
type WindowCond struct {
	UsedBelow    *float64 `yaml:"used_below"`
	UsedAtLeast  *float64 `yaml:"used_at_least"`
	ResetsWithin string   `yaml:"resets_within"`
}

// Pick orders.
const (
	OrderListed       = "listed"
	OrderSoonestReset = "soonest_reset"
	OrderLeastUsed    = "least_used"
)

const (
	defaultStickyTTL = 6 * time.Hour
	maxTreeDepth     = 32
)

var dayIndex = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

type matcher struct {
	model, provider, client, session *regexp.Regexp
	headers                          map[string]*regexp.Regexp
	// window is [start, end) in minutes since midnight; it wraps midnight when end <= start.
	window *[2]int
	days   map[time.Weekday]bool
}

type route struct {
	name     string
	match    matcher
	provider string
	model    string
}

type nodeKind int

const (
	kindBuiltin nodeKind = iota
	kindIf
	kindPick
	kindDelegate
	kindReject
)

type node struct {
	// id is the node's path from the root, e.g. tree.then.else; the UI uses it to highlight traces.
	id, name   string
	kind       nodeKind
	cond       matcher
	then, els  *node
	accounts   []*regexp.Regexp
	where      Where
	within     map[string]time.Duration
	order      string
	window     string
	sticky     bool
	delegate   string
	rejectText string
}

// Parse decodes and validates config YAML. Unknown fields are rejected so typos surface at load
// time. State and usage are shared so pins and readings survive a reconfigure.
func Parse(raw []byte, state *State, usage UsageSource) (*Router, error) {
	var cfg Config
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}

	r := &Router{loc: time.Local, stickyTTL: defaultStickyTTL, state: state, usage: usage, clients: map[string]string{}}
	var errs []error
	if cfg.Timezone != "" {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			errs = append(errs, fmt.Errorf("timezone: %w", err))
		} else {
			r.loc = loc
		}
	}
	if cfg.StickyTTL != "" {
		ttl, err := time.ParseDuration(cfg.StickyTTL)
		if err != nil || ttl <= 0 {
			errs = append(errs, fmt.Errorf("sticky_ttl %q: must be a positive duration like 6h", cfg.StickyTTL))
		} else {
			r.stickyTTL = ttl
		}
	}
	if cfg.Usage.Interval != "" {
		d, err := time.ParseDuration(cfg.Usage.Interval)
		if err != nil || d < time.Minute {
			errs = append(errs, fmt.Errorf("usage.interval %q: must be a duration of at least 1m", cfg.Usage.Interval))
		}
		r.usageSettings.Interval = d
	}
	r.usageSettings.ClaudeURL = cfg.Usage.ClaudeURL
	r.usageSettings.CodexURL = cfg.Usage.CodexURL
	for _, h := range cfg.SessionHeaders {
		if h = strings.TrimSpace(h); h != "" {
			r.sessionHeaders = append(r.sessionHeaders, h)
		}
	}
	for name, digest := range cfg.Clients {
		digest = strings.ToLower(strings.TrimSpace(digest))
		if b, err := hex.DecodeString(digest); err != nil || len(b) != 32 {
			errs = append(errs, fmt.Errorf("clients.%s: must be the hex SHA-256 of the client key", name))
			continue
		}
		r.clients[digest] = name
	}

	for i, rule := range cfg.Routes {
		name := ruleName(rule.Name, fmt.Sprintf("routes[%d]", i))
		m, err := compileCond(rule.Match)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if strings.TrimSpace(rule.Provider) == "" {
			errs = append(errs, fmt.Errorf("%s: provider is required", name))
			continue
		}
		r.routes = append(r.routes, route{
			name:     name,
			match:    m,
			provider: strings.ToLower(strings.TrimSpace(rule.Provider)),
			model:    strings.TrimSpace(rule.Model),
		})
	}
	if cfg.Tree != nil {
		root, treeErrs := compileNode(cfg.Tree, "tree", 0)
		errs = append(errs, treeErrs...)
		r.tree = root
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return r, nil
}

// UsageSettings returns the polling settings from the config.
func (r *Router) UsageSettings() usage.Settings { return r.usageSettings }

func ruleName(name, fallback string) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return fallback
}

func compileNode(n *Node, id string, depth int) (*node, []error) {
	if n == nil {
		return &node{id: id, name: id, kind: kindBuiltin}, nil
	}
	label := ruleName(n.Name, id)
	if depth > maxTreeDepth {
		return nil, []error{fmt.Errorf("%s: tree is deeper than %d levels", label, maxTreeDepth)}
	}
	out := &node{id: id, name: label, sticky: n.Sticky}
	set := 0
	for _, isSet := range []bool{n.If != nil, n.Pick != nil, n.Delegate != "", n.Reject != "", n.Builtin} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return nil, []error{fmt.Errorf("%s: set exactly one of if, pick, delegate, reject, builtin", label)}
	}
	var errs []error
	addChild := func(child *Node, suffix string) *node {
		c, childErrs := compileNode(child, id+"."+suffix, depth+1)
		errs = append(errs, childErrs...)
		return c
	}
	switch {
	case n.If != nil:
		out.kind = kindIf
		m, err := compileCond(*n.If)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
		out.cond = m
		out.then = addChild(n.Then, "then")
		out.els = addChild(n.Else, "else")
	case n.Pick != nil:
		out.kind = kindPick
		for _, g := range n.Pick.Accounts {
			out.accounts = append(out.accounts, compileGlob(g))
		}
		out.order = n.Pick.Order
		switch out.order {
		case "":
			out.order = OrderListed
		case OrderListed, OrderSoonestReset, OrderLeastUsed:
		default:
			errs = append(errs, fmt.Errorf("%s: order %q must be listed, soonest_reset, or least_used", label, out.order))
		}
		out.window = n.Pick.Window
		switch out.window {
		case "":
			out.window = "weekly"
		case "weekly", "five_hour":
		default:
			errs = append(errs, fmt.Errorf("%s: window %q must be weekly or five_hour", label, out.window))
		}
		if n.Pick.Where != nil {
			out.where = *n.Pick.Where
			out.within = map[string]time.Duration{}
			for name, w := range map[string]*WindowCond{"weekly": out.where.Weekly, "five_hour": out.where.FiveHour} {
				if w == nil || w.ResetsWithin == "" {
					continue
				}
				d, err := time.ParseDuration(w.ResetsWithin)
				if err != nil || d <= 0 {
					errs = append(errs, fmt.Errorf("%s: where.%s.resets_within %q must be a positive duration like 24h", label, name, w.ResetsWithin))
				}
				out.within[name] = d
			}
		}
		out.els = addChild(n.Else, "else")
	case n.Delegate != "":
		out.kind = kindDelegate
		out.delegate = n.Delegate
		if n.Delegate != "round-robin" && n.Delegate != "fill-first" {
			errs = append(errs, fmt.Errorf("%s: delegate %q must be round-robin or fill-first", label, n.Delegate))
		}
	case n.Reject != "":
		out.kind = kindReject
		out.rejectText = n.Reject
	default:
		out.kind = kindBuiltin
	}
	if n.Sticky && out.kind != kindPick {
		errs = append(errs, fmt.Errorf("%s: sticky only applies to pick nodes", label))
	}
	if (n.Then != nil) && out.kind != kindIf {
		errs = append(errs, fmt.Errorf("%s: then only applies to if nodes", label))
	}
	if (n.Else != nil) && out.kind != kindIf && out.kind != kindPick {
		errs = append(errs, fmt.Errorf("%s: else only applies to if and pick nodes", label))
	}
	return out, errs
}

func compileCond(m Cond) (matcher, error) {
	out := matcher{
		model:    compileOptionalGlob(m.Model),
		provider: compileOptionalGlob(m.Provider),
		client:   compileOptionalGlob(m.Client),
		session:  compileOptionalGlob(m.Session),
	}
	if len(m.Headers) > 0 {
		out.headers = make(map[string]*regexp.Regexp, len(m.Headers))
		for k, v := range m.Headers {
			out.headers[k] = compileGlob(v)
		}
	}
	if m.Hours != "" {
		w, err := parseWindow(m.Hours)
		if err != nil {
			return matcher{}, err
		}
		out.window = &w
	}
	if len(m.Days) > 0 {
		out.days = make(map[time.Weekday]bool, len(m.Days))
		for _, d := range m.Days {
			wd, ok := dayIndex[strings.ToLower(strings.TrimSpace(d))]
			if !ok {
				return matcher{}, fmt.Errorf("day %q must be one of sun, mon, tue, wed, thu, fri, sat", d)
			}
			out.days[wd] = true
		}
	}
	return out, nil
}

func parseWindow(s string) ([2]int, error) {
	start, end, ok := strings.Cut(s, "-")
	if !ok {
		return [2]int{}, fmt.Errorf("hours %q must look like 09:00-17:30", s)
	}
	a, errA := parseClock(start)
	b, errB := parseClock(end)
	if errA != nil || errB != nil {
		return [2]int{}, fmt.Errorf("hours %q must look like 09:00-17:30", s)
	}
	return [2]int{a, b}, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

func compileOptionalGlob(g string) *regexp.Regexp {
	if strings.TrimSpace(g) == "" {
		return nil
	}
	return compileGlob(g)
}

// compileGlob supports * (any run, including /) and ? (one character), case-insensitively.
func compileGlob(g string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(strings.TrimSpace(g))
	quoted = strings.ReplaceAll(quoted, `\*`, ".*")
	quoted = strings.ReplaceAll(quoted, `\?`, ".")
	return regexp.MustCompile("(?i)^" + quoted + "$")
}
