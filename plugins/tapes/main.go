// Command tapes is a CLIProxyAPI native plugin that records every proxied
// LLM request/response to a tapes ingest server. Build with
// -buildmode=c-shared; the C ABI lives in internal/cabi.
package main

import "github.com/lachieh/cliproxyapi-plugins/internal/cabi"

var instance = newPlugin(cabi.Log)

func init() {
	cabi.Serve(cabi.Plugin{Handle: instance.handle, Shutdown: instance.shutdown})
}

func main() {}
