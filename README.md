<img src="assets/icon.svg" width="64" height="64" alt="">

# cliproxyapi-plugin-tapes

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that records every request and response passing through the proxy into [tapes](https://tapes.dev), so the sessions of every coding agent that uses the proxy show up in tapes.

The plugin captures the conversation exactly as the client sent and received it, streaming or not, and sends it to tapes' ingest API (`POST /v1/ingest`). Delivery happens on a background queue, so a slow or unreachable tapes server never delays proxied requests.

## Install

You need a CLIProxyAPI build with plugin support (the official Docker image and the default release binaries have it) and plugins turned on in `config.yaml`:

```yaml
plugins:
  enabled: true
```

### From the plugin store

Add this repository's registry as a store source:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/lachieh/cliproxyapi-plugin-tapes/main/registry.json"
```

The plugin then appears in the management panel's plugin store as **tapes**. Install it there, or through the management API:

```sh
curl -X POST -H "Authorization: Bearer $MANAGEMENT_KEY" \
  "http://localhost:8317/v0/management/plugin-store/tapes/install"
```

### Manually

Download the zip for your platform from the [latest release](https://github.com/lachieh/cliproxyapi-plugin-tapes/releases/latest) and extract the library into `plugins/<os>/<arch>/`, for example `plugins/linux/amd64/tapes.so`. Then restart CLIProxyAPI.

## Configure

Settings go under `plugins.configs.tapes`, and can also be edited in the management panel:

```yaml
plugins:
  enabled: true
  configs:
    tapes:
      enabled: true
      ingest-url: "http://tapes.homelab.lan:8082"
      keys:
        laptop: "sk-cpa-laptop-..."   # a key from access.api-keys
        homelab-ci: "sk-cpa-ci-..."
```

| Key | Default | Description |
| --- | --- | --- |
| `ingest-url` | `http://localhost:8082` | Your tapes ingest API. A base URL gets `/v1/ingest` appended. |
| `keys` | none | Names for your client API keys (`name: key`). The name is recorded as the session's auth subject and in the agent name. The keys themselves are never sent to tapes. |
| `headers` | none | Extra headers for each ingest request, such as `Authorization` for a gateway in front of tapes. |
| `timeout-seconds` | `10` | Timeout for each ingest request. |
| `queue-size` | `256` | Turns buffered while tapes is slow. Turns beyond this are dropped and a warning is logged. |

Changes apply without a restart. An invalid config is logged and the previous settings stay in effect.

### Pointing at tapes

There is no hosted tapes ingest service. tapes runs where you run it, and its ingest API listens on port 8082 by default. That is why the default is `http://localhost:8082`, which works when tapes runs alongside CLIProxyAPI. For a homelab server, set `ingest-url` to its address.

To run tapes, see the [installation guide](https://tapes.dev/docs/installation/). `tapes serve` starts the ingest API along with the read API and the worker that turns captured requests into sessions.

> Use the **ingest** port (8082), not the read API port (8081). A capture sent to the read port reports success but stores nothing.

The ingest API has no authentication of its own, and tapes documents it as a private, in-cluster endpoint. Keep it on a trusted network, or put a gateway in front of it and set `headers`.

## What gets recorded

The plugin records requests in the protocols tapes understands:

| Client protocol | Recorded as |
| --- | --- |
| OpenAI Chat Completions (`/v1/chat/completions`) | `openai` |
| OpenAI Responses (`/v1/responses`) | `openai` |
| Anthropic Messages (`/v1/messages`) | `anthropic` |

Each turn is recorded in the client's protocol, whichever upstream served it. A Claude Code request routed to a Gemini model is therefore still a Claude session in tapes.

These requests are **not** recorded:

- Requests in the Gemini protocol, which tapes cannot parse.
- Requests that fail before producing a response.
- Responses larger than 8 MiB, which is tapes' limit.

A stream the client abandons partway is recorded with whatever it produced.

### Who and where

The plugin fills tapes' own session fields from what coding agents already send:

| tapes field | Value |
| --- | --- |
| `harness_id` | The coding tool, detected from `User-Agent`: `claude`, `codex`, `opencode` or `gemini`. Unrecognized clients get `cliproxyapi`. tapes' own `unknown` can't be used, because tapes never derives sessions for `unknown` turns sent through its ingest API. |
| `harness_version` | The tool's version, from `User-Agent`. |
| `auth_subject` | The name of the client API key, from `keys`. |
| `cwd` | The project directory, read from the prompt (Claude Code, OpenCode and Codex state it). |
| `agent_name` | `<key name>/<tool>`, e.g. `laptop/claude`. If only one is known, just that one; if neither, `unknown`. |
| `harness_session_id` | Taken from the first of these that is available. |

The session ID sources, in order:

1. Claude Code's session ID, from the `X-Claude-Code-Session-Id` header or `metadata.user_id`. Subagent turns are also tagged with their agent ID.
2. Codex's `session_id` header or `prompt_cache_key`.
3. A generic `X-Session-Id` header.
4. Otherwise, a hash of the conversation's system prompt and first message. The hash stays the same as later turns resend the history, so they land in the same session.

A client can set any of these explicitly with the headers tapes' own proxy reads. They take precedence over detection:

- `X-Tapes-Agent-Name`
- `X-Tapes-Harness-Id`
- `X-Tapes-Harness-Version`
- `X-Tapes-Harness-Session-Id`
- `X-Tapes-Parent-Harness-Session-Id`
- `X-Tapes-Cwd`

## Develop

```sh
go test -race ./...
go build -buildmode=c-shared -o plugins/tapes.dylib .   # .so on Linux, .dll on Windows
```

The plugin is a Go `c-shared` library loaded through CLIProxyAPI's C ABI. It uses three hooks:

- `response.intercept_after` captures non-streaming responses.
- `response.intercept_stream_chunk` captures streamed chunks.
- `request.complete` sends the finished turn.

The interceptors return no changes, so they never alter traffic.

The code is in three packages:

| Package | Purpose |
| --- | --- |
| `internal/capture` | Assembles the hook calls into complete exchanges. |
| `internal/tapes` | Builds ingest payloads and delivers them. |
| `internal/config` | Parses the plugin config. |

### Release

Push a `v*` tag. The workflow builds linux (amd64, arm64), darwin (amd64, arm64) and windows (amd64), then publishes the store-format zips and `checksums.txt` to a GitHub release. Linux builds run on Debian bookworm to match the official CLIProxyAPI image.

## License

MIT
