// Package usage fetches and normalizes subscription usage for OAuth accounts so routing rules
// can compare Claude and Codex accounts on the same terms.
package usage

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Window is one rate-limit window.
type Window struct {
	// UsedPercent is 0-100.
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// Credits is paid usage beyond the plan allowance.
type Credits struct {
	Available bool   `json:"available"`
	Detail    string `json:"detail,omitempty"`
}

// Usage is the normalized usage of one account. Nil fields were not reported.
type Usage struct {
	FiveHour *Window  `json:"five_hour,omitempty"`
	Weekly   *Window  `json:"weekly,omitempty"`
	Credits  *Credits `json:"credits,omitempty"`
}

// Endpoints the providers' own clients use to show usage.
const (
	ClaudeURL = "https://api.anthropic.com/api/oauth/usage"
	CodexURL  = "https://chatgpt.com/backend-api/wham/usage"
)

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type claudePayload struct {
	FiveHour   *claudeWindow `json:"five_hour"`
	SevenDay   *claudeWindow `json:"seven_day"`
	ExtraUsage *struct {
		IsEnabled    bool    `json:"is_enabled"`
		MonthlyLimit float64 `json:"monthly_limit"`
		UsedCredits  float64 `json:"used_credits"`
	} `json:"extra_usage"`
}

// ParseClaude normalizes an /api/oauth/usage response.
func ParseClaude(body []byte) (Usage, error) {
	var p claudePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Usage{}, fmt.Errorf("decode claude usage: %w", err)
	}
	var u Usage
	u.FiveHour = p.FiveHour.normalize()
	u.Weekly = p.SevenDay.normalize()
	if e := p.ExtraUsage; e != nil {
		remaining := e.MonthlyLimit - e.UsedCredits
		u.Credits = &Credits{
			Available: e.IsEnabled && remaining > 0,
			Detail:    fmt.Sprintf("%s of %s monthly extra usage left", money(remaining), money(e.MonthlyLimit)),
		}
		if !e.IsEnabled {
			u.Credits.Detail = "extra usage disabled"
		}
	}
	return u, nil
}

func (w *claudeWindow) normalize() *Window {
	if w == nil || w.Utilization == nil {
		return nil
	}
	out := &Window{UsedPercent: *w.Utilization}
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		out.ResetsAt = t
	}
	return out
}

// Claude reports extra usage in cents.
func money(cents float64) string {
	return fmt.Sprintf("$%.2f", math.Max(cents, 0)/100)
}

type codexWindow struct {
	UsedPercent        flexNumber `json:"used_percent"`
	LimitWindowSeconds flexNumber `json:"limit_window_seconds"`
	ResetAfterSeconds  flexNumber `json:"reset_after_seconds"`
	ResetAt            flexNumber `json:"reset_at"`
}

type codexPayload struct {
	RateLimit *struct {
		Primary   *codexWindow `json:"primary_window"`
		Secondary *codexWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool            `json:"has_credits"`
		Unlimited  bool            `json:"unlimited"`
		Balance    json.RawMessage `json:"balance"`
	} `json:"credits"`
}

const (
	fiveHourSeconds = 5 * 60 * 60
	weekSeconds     = 7 * 24 * 60 * 60
)

// ParseCodex normalizes a /wham/usage response. Windows are classified by length rather than
// position because the primary window is not always the five-hour one.
func ParseCodex(body []byte, now time.Time) (Usage, error) {
	var p codexPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Usage{}, fmt.Errorf("decode codex usage: %w", err)
	}
	var u Usage
	if rl := p.RateLimit; rl != nil {
		for _, w := range []*codexWindow{rl.Primary, rl.Secondary} {
			if w == nil || !w.UsedPercent.ok {
				continue
			}
			out := &Window{UsedPercent: w.UsedPercent.v}
			switch {
			case w.ResetAt.ok && w.ResetAt.v > 0:
				out.ResetsAt = time.Unix(int64(w.ResetAt.v), 0).UTC()
			case w.ResetAfterSeconds.ok:
				out.ResetsAt = now.Add(time.Duration(w.ResetAfterSeconds.v) * time.Second).UTC()
			}
			switch {
			case w.LimitWindowSeconds.v >= weekSeconds:
				u.Weekly = out
			case w.LimitWindowSeconds.v > 0 && w.LimitWindowSeconds.v <= fiveHourSeconds:
				u.FiveHour = out
			}
		}
	}
	if c := p.Credits; c != nil {
		balance := strings.Trim(string(c.Balance), `"`)
		n, _ := strconv.ParseFloat(balance, 64)
		u.Credits = &Credits{Available: c.Unlimited || (c.HasCredits && n > 0)}
		switch {
		case c.Unlimited:
			u.Credits.Detail = "unlimited credits"
		case c.HasCredits:
			u.Credits.Detail = "credit balance " + balance
		default:
			u.Credits.Detail = "no credits"
		}
	}
	return u, nil
}

// flexNumber accepts a JSON number or numeric string; Codex sends both.
type flexNumber struct {
	v  float64
	ok bool
}

func (f *flexNumber) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	f.v, f.ok = v, true
	return nil
}
