package main

import (
	"embed"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/lachieh/cliproxyapi-plugin-lachieh/internal/router"
)

// The UI is a static shell served as unauthenticated plugin resources. Everything it reads or
// writes goes through the management-key-protected routes below.
//
//go:embed web
var web embed.FS

const (
	apiBase      = "/plugins/" + pluginID
	resourceBase = "/v0/resource/plugins/" + pluginID
)

type asset struct{ file, contentType string }

var assets = map[string]asset{
	"/ui":     {"web/index.html", "text/html; charset=utf-8"},
	"/ui.js":  {"web/app.js", "text/javascript; charset=utf-8"},
	"/ui.css": {"web/app.css", "text/css; charset=utf-8"},
}

func managementRegistration() any {
	type route struct{ Method, Path string }
	type resource struct{ Path, Menu, Description string }
	return struct {
		Routes    []route
		Resources []resource
	}{
		Routes: []route{
			{http.MethodGet, apiBase + "/state"},
			{http.MethodPost, apiBase + "/validate"},
			{http.MethodPost, apiBase + "/simulate"},
			{http.MethodPost, apiBase + "/usage/refresh"},
		},
		Resources: []resource{
			{"/ui", "Routing", "Edit the account decision tree and see live usage."},
			{"/ui.js", "", ""},
			{"/ui.css", "", ""},
		},
	}
}

func handleManagement(req pluginapi.ManagementRequest, now time.Time) pluginapi.ManagementResponse {
	if rest, ok := strings.CutPrefix(req.Path, resourceBase); ok {
		return serveAsset(rest)
	}
	_, endpoint, _ := strings.Cut(req.Path, apiBase)
	switch req.Method + " " + endpoint {
	case "GET /state":
		return jsonResponse(http.StatusOK, map[string]any{
			"version":    version,
			"configured": current.Load() != nil,
			"state":      state.Snapshot(now),
			"usage":      poller.Accounts(),
		})
	case "POST /validate":
		if _, err := router.Parse(req.Body, state, poller); err != nil {
			return jsonResponse(http.StatusOK, map[string]any{"ok": false, "errors": splitErrors(err)})
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "errors": []string{}})
	case "POST /simulate":
		var body struct {
			Config json.RawMessage `json:"config"`
			Input  router.SimInput `json:"input"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonResponse(http.StatusBadRequest, map[string]any{"errors": []string{err.Error()}})
		}
		r, err := loaded()
		if len(body.Config) > 0 {
			r, err = router.Parse(body.Config, state, poller)
		}
		if err != nil {
			return jsonResponse(http.StatusOK, map[string]any{"errors": splitErrors(err)})
		}
		if len(body.Input.Candidates) == 0 {
			body.Input.Candidates = knownAccounts(body.Input.Provider, now)
		}
		return jsonResponse(http.StatusOK, r.Simulate(body.Input, now))
	case "POST /usage/refresh":
		poller.Refresh(now)
		return jsonResponse(http.StatusOK, poller.Accounts())
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}
}

// knownAccounts lists accounts seen in traffic or by the usage poller, optionally for one provider.
func knownAccounts(provider string, now time.Time) []string {
	seen := map[string]string{}
	for _, a := range state.Snapshot(now).Accounts {
		seen[a.ID] = a.Provider
	}
	for _, a := range poller.Accounts() {
		seen[a.ID] = a.Provider
	}
	var ids []string
	for id, p := range seen {
		if provider == "" || strings.EqualFold(p, provider) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func serveAsset(path string) pluginapi.ManagementResponse {
	a, ok := assets[path]
	if !ok {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}
	}
	body, err := web.ReadFile(a.file)
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound}
	}
	return pluginapi.ManagementResponse{
		Headers: http.Header{
			"Content-Type":  {a.contentType},
			"Cache-Control": {"no-cache"},
			// The page loads only its own assets and talks only to this origin.
			"Content-Security-Policy": {"default-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'self'"},
		},
		Body: body,
	}
}

func jsonResponse(status int, v any) pluginapi.ManagementResponse {
	body, err := json.Marshal(v)
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError}
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json"}},
		Body:       body,
	}
}

func splitErrors(err error) []string {
	return strings.Split(err.Error(), "\n")
}
