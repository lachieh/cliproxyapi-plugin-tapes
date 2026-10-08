// Package router holds the routing rules and decisions, free of the C ABI.
package router

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

	Routes   []RouteRule   `yaml:"routes"`
	Accounts []AccountRule `yaml:"accounts"`
}

// Match conditions are ANDed; an empty field always matches.
type Match struct {
	Model    string            `yaml:"model"`
	Provider string            `yaml:"provider"`
	Headers  map[string]string `yaml:"headers"`
	Session  string            `yaml:"session"`
	Hours    string            `yaml:"hours"`
	Days     []string          `yaml:"days"`
}

// RouteRule sends matching requests to a provider before account selection.
type RouteRule struct {
	Name     string `yaml:"name"`
	Match    Match  `yaml:"match"`
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

// AccountRule picks an account for matching requests.
type AccountRule struct {
	Name     string   `yaml:"name"`
	Match    Match    `yaml:"match"`
	Prefer   []string `yaml:"prefer"`
	Sticky   bool     `yaml:"sticky"`
	Fallback string   `yaml:"fallback"`
}

// Fallback values for AccountRule when no preferred account is available.
const (
	FallbackBuiltin    = "builtin"
	FallbackRoundRobin = "round-robin"
	FallbackFillFirst  = "fill-first"
	FallbackReject     = "reject"
)

const defaultStickyTTL = 6 * time.Hour

var dayIndex = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

type matcher struct {
	model, provider, session *regexp.Regexp
	headers                  map[string]*regexp.Regexp
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

type account struct {
	name     string
	match    matcher
	prefer   []*regexp.Regexp
	sticky   bool
	fallback string
}

// Parse decodes and validates config YAML. Unknown fields are rejected so typos surface at load time.
// State is passed in so session pins and observations survive a reconfigure.
func Parse(raw []byte, state *State) (*Router, error) {
	var cfg Config
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}

	r := &Router{loc: time.Local, stickyTTL: defaultStickyTTL, state: state}
	if cfg.Timezone != "" {
		loc, err := time.LoadLocation(cfg.Timezone)
		if err != nil {
			return nil, fmt.Errorf("timezone: %w", err)
		}
		r.loc = loc
	}
	if cfg.StickyTTL != "" {
		ttl, err := time.ParseDuration(cfg.StickyTTL)
		if err != nil || ttl <= 0 {
			return nil, fmt.Errorf("sticky_ttl %q: must be a positive duration like 6h", cfg.StickyTTL)
		}
		r.stickyTTL = ttl
	}
	for _, h := range cfg.SessionHeaders {
		if h = strings.TrimSpace(h); h != "" {
			r.sessionHeaders = append(r.sessionHeaders, h)
		}
	}

	var errs []error
	for i, rule := range cfg.Routes {
		name := ruleName(rule.Name, "routes", i)
		m, err := compileMatch(rule.Match)
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
	for i, rule := range cfg.Accounts {
		name := ruleName(rule.Name, "accounts", i)
		m, err := compileMatch(rule.Match)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		fallback := rule.Fallback
		switch fallback {
		case "":
			fallback = FallbackBuiltin
		case FallbackBuiltin, FallbackRoundRobin, FallbackFillFirst, FallbackReject:
		default:
			errs = append(errs, fmt.Errorf("%s: fallback %q must be one of builtin, round-robin, fill-first, reject", name, fallback))
			continue
		}
		a := account{name: name, match: m, sticky: rule.Sticky, fallback: fallback}
		for _, p := range rule.Prefer {
			a.prefer = append(a.prefer, compileGlob(p))
		}
		if len(a.prefer) == 0 && !a.sticky && fallback == FallbackBuiltin {
			errs = append(errs, fmt.Errorf("%s: rule has no effect; set prefer, sticky, or a non-builtin fallback", name))
			continue
		}
		r.accounts = append(r.accounts, a)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return r, nil
}

func ruleName(name, section string, i int) string {
	if name = strings.TrimSpace(name); name != "" {
		return name
	}
	return fmt.Sprintf("%s[%d]", section, i)
}

func compileMatch(m Match) (matcher, error) {
	out := matcher{
		model:    compileOptionalGlob(m.Model),
		provider: compileOptionalGlob(m.Provider),
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
