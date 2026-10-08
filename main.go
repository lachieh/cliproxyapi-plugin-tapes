// Command tapes is a CLIProxyAPI native plugin that records every proxied
// LLM request/response to a tapes ingest server. Build with
// -buildmode=c-shared; the host loads it through the C ABI below.
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

static void store_host_api(const cliproxy_host_api* host) { stored_host = host; }

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) { return 1; }
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) { stored_host->free_buffer(ptr, len); }
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

const abiVersion = 1

var (
	// hostAvailable guards host callbacks: the host frees its API table on unload.
	hostAvailable atomic.Bool
	instance      = newPlugin(hostLog)
)

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	if host != nil {
		C.store_host_api(host)
		hostAvailable.Store(true)
	}
	plugin.abi_version = C.uint32_t(abiVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	// A panic would cross the cgo boundary and take the whole proxy down.
	defer func() {
		if r := recover(); r != nil {
			writeResponse(response, errorEnvelope("plugin_panic", fmt.Sprint(r)))
			code = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var body []byte
	if request != nil && requestLen > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := dispatch(C.GoString(method), body)
	writeResponse(response, raw)
	if err != nil {
		return 1
	}
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	instance.shutdown()
	hostAvailable.Store(false)
}

// dispatch runs a host call and encodes the result as a response envelope.
func dispatch(method string, request []byte) ([]byte, error) {
	result, err := instance.handle(method, request)
	if err != nil {
		code := "plugin_error"
		if errors.As(err, new(errUnknownMethod)) {
			code = "unknown_method"
		}
		return errorEnvelope(code, err.Error()), err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return errorEnvelope("plugin_error", err.Error()), err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	response.ptr = C.CBytes(raw)
	response.len = C.size_t(len(raw))
}

// hostLog writes to the CLIProxyAPI log through the host.log callback.
func hostLog(level, message string, fields map[string]any) {
	if !hostAvailable.Load() {
		return
	}
	payload, err := json.Marshal(map[string]any{"level": level, "message": message, "fields": fields})
	if err != nil {
		return
	}
	cMethod := C.CString("host.log")
	defer C.free(unsafe.Pointer(cMethod))
	req := (*C.uint8_t)(C.CBytes(payload))
	defer C.free(unsafe.Pointer(req))
	var response C.cliproxy_buffer
	C.call_host_api(cMethod, req, C.size_t(len(payload)), &response)
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
}
