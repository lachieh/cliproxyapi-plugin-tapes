// Command lachieh-router is a CLIProxyAPI plugin that routes requests to providers and
// accounts by personal preference. Build with -buildmode=c-shared; the C ABI lives in internal/cabi.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/lachieh/cliproxyapi-plugins/internal/cabi"
	"github.com/lachieh/cliproxyapi-plugins/plugins/router/internal/router"
	"github.com/lachieh/cliproxyapi-plugins/plugins/router/internal/usage"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const pluginID = "lachieh-router"

var (
	current atomic.Pointer[router.Router]
	state   = router.NewState()
	poller  = usage.NewPoller(hostBridge{})

	startPoller sync.Once
	stopPoller  context.CancelFunc = func() {}
)

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

type capabilities struct {
	Scheduler     bool `json:"scheduler"`
	ModelRouter   bool `json:"model_router"`
	ManagementAPI bool `json:"management_api"`
}

func init() {
	cabi.Serve(cabi.Plugin{Handle: handle, Shutdown: func() { stopPoller() }})
}

func main() {}

func handle(method string, body []byte) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, err
			}
		}
		// An invalid config is reported to the host, which unregisters the plugin until it is fixed.
		r, err := router.Parse(req.ConfigYAML, state, poller)
		if err != nil {
			return nil, &cabi.Error{Code: "invalid_config", Message: err.Error()}
		}
		current.Store(r)
		poller.Configure(r.UsageSettings())
		startPoller.Do(func() {
			ctx, cancel := context.WithCancel(context.Background())
			stopPoller = cancel
			go poller.Run(ctx)
		})
		return manifest(), nil
	case pluginabi.MethodPluginShutdown:
		stopPoller()
		return struct{}{}, nil
	case pluginabi.MethodModelRoute:
		var req pluginapi.ModelRouteRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		r, err := loaded()
		if err != nil {
			return nil, err
		}
		return r.Route(req, time.Now()), nil
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		r, err := loaded()
		if err != nil {
			return nil, err
		}
		return r.Pick(req, time.Now()), nil
	case pluginabi.MethodManagementRegister:
		return managementRegistration(), nil
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return handleManagement(req, time.Now()), nil
	default:
		return nil, &cabi.Error{Code: "unknown_method", Message: "unknown method: " + method}
	}
}

func loaded() (*router.Router, error) {
	if r := current.Load(); r != nil {
		return r, nil
	}
	return nil, errors.New("plugin is not configured")
}

func manifest() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          version,
			Author:           "lachieh",
			GitHubRepository: "https://github.com/lachieh/cliproxyapi-plugins",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "IANA zone for hours/days rules. Defaults to the host local zone."},
				{Name: "session_headers", Type: pluginapi.ConfigFieldTypeArray, Description: "Headers checked for a session ID after host session metadata."},
				{Name: "sticky_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long an idle session stays pinned to its account, e.g. 6h."},
				{Name: "clients", Type: pluginapi.ConfigFieldTypeObject, Description: "Client key names: name -> hex SHA-256 of the key. Easier from the Routing page."},
				{Name: "usage", Type: pluginapi.ConfigFieldTypeObject, Description: "Usage polling: interval (default 5m)."},
				{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Ordered provider/model routing rules; first match wins."},
				{Name: "tree", Type: pluginapi.ConfigFieldTypeObject, Description: "Account decision tree. Edit it on the Routing page."},
			},
		},
		Capabilities: capabilities{Scheduler: true, ModelRouter: true, ManagementAPI: true},
	}
}
