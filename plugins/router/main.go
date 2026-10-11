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

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
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

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/lachieh/cliproxyapi-plugin-lachieh/internal/router"
	"github.com/lachieh/cliproxyapi-plugin-lachieh/internal/usage"
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
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
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
func cliproxyPluginShutdown() { stopPoller() }

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
		r, err := router.Parse(req.ConfigYAML, state, poller)
		if err != nil {
			return errorEnvelope("invalid_config", err.Error()), nil
		}
		current.Store(r)
		poller.Configure(r.UsageSettings())
		startPoller.Do(func() {
			ctx, cancel := context.WithCancel(context.Background())
			stopPoller = cancel
			go poller.Run(ctx)
		})
		return okEnvelope(manifest())
	case pluginabi.MethodPluginShutdown:
		stopPoller()
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
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return okEnvelope(handleManagement(req, time.Now()))
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
				{Name: "clients", Type: pluginapi.ConfigFieldTypeObject, Description: "Client key names: name -> hex SHA-256 of the key. Easier from the Routing page."},
				{Name: "usage", Type: pluginapi.ConfigFieldTypeObject, Description: "Usage polling: interval (default 5m)."},
				{Name: "routes", Type: pluginapi.ConfigFieldTypeArray, Description: "Ordered provider/model routing rules; first match wins."},
				{Name: "tree", Type: pluginapi.ConfigFieldTypeObject, Description: "Account decision tree. Edit it on the Routing page."},
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

// callHost invokes a host callback. It is safe to call from any goroutine.
func callHost(method string, payload any) (json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var reqPtr *C.uint8_t
	if len(raw) > 0 {
		p := C.CBytes(raw)
		defer C.free(p)
		reqPtr = (*C.uint8_t)(p)
	}
	var response C.cliproxy_buffer
	code := C.call_host_api(cMethod, reqPtr, C.size_t(len(raw)), &response)
	var out []byte
	if response.ptr != nil {
		out = C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no response (code %d)", method, int(code))
	}
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s failed", method)
	}
	return env.Result, nil
}
