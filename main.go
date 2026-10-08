// Command lachieh-router is a CLIProxyAPI plugin that routes requests to providers and
// accounts by personal preference. Build with -buildmode=c-shared; see the Makefile.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/lachieh/cliproxyapi-plugin-lachieh/internal/router"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	pluginID  = "lachieh-router"
	statePath = "/plugins/" + pluginID + "/state"
)

var (
	current atomic.Pointer[router.Router]
	state   = router.NewState()
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

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

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var body []byte
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handle(C.GoString(method), body)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handle(method string, body []byte) ([]byte, error) {
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
		r, err := router.Parse(req.ConfigYAML, state)
		if err != nil {
			return errorEnvelope("invalid_config", err.Error()), nil
		}
		current.Store(r)
		return okEnvelope(manifest())
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodModelRoute:
		var req pluginapi.ModelRouteRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		r, err := loaded()
		if err != nil {
			return nil, err
		}
		return okEnvelope(r.Route(req, time.Now()))
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		r, err := loaded()
		if err != nil {
			return nil, err
		}
		return okEnvelope(r.Pick(req, time.Now()))
	case pluginabi.MethodManagementRegister:
		type route struct{ Method, Path string }
		return okEnvelope(map[string][]route{"routes": {{Method: http.MethodGet, Path: statePath}}})
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		if req.Method != http.MethodGet || !strings.HasSuffix(req.Path, statePath) {
			return okEnvelope(pluginapi.ManagementResponse{StatusCode: http.StatusNotFound})
		}
		snapshot, err := json.MarshalIndent(state.Snapshot(time.Now()), "", "  ")
		if err != nil {
			return nil, err
		}
		return okEnvelope(pluginapi.ManagementResponse{
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    snapshot,
		})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
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
			GitHubRepository: "https://github.com/lachieh/cliproxyapi-plugin-lachieh",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "timezone", Type: pluginapi.ConfigFieldTypeString, Description: "IANA zone for hours/days rules. Defaults to the host local zone."},
				{Name: "session_headers", Type: pluginapi.ConfigFieldTypeArray, Description: "Headers checked for a session ID after host session metadata."},
				{Name: "sticky_ttl", Type: pluginapi.ConfigFieldTypeString, Description: "How long an idle session stays pinned to its account, e.g. 6h."},
				{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Ordered provider/model routing rules; first match wins."},
				{Name: "accounts", Type: pluginapi.ConfigFieldTypeArray, Description: "Ordered account preference rules; first match wins."},
			},
		},
		Capabilities: capabilities{Scheduler: true, ModelRouter: true, ManagementAPI: true},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
