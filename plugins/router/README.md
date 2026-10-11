<img src="assets/icon.svg" width="64" height="64" alt="">

# lachieh-router

A personal [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin, `lachieh-router`, that routes sessions by my preferences:

- **Model routes** (`model.route`): before account selection, send matching requests to another provider and optionally rename the model.
- **Decision tree** (`scheduler.pick`): choose which account serves a request using request conditions (model, provider, client key, headers, session, time) and each account's live **usage** (5-hour and weekly windows, paid credit).

Edit both in the management panel under **Plugins → Routing**.

## The Routing page

Open the management panel and choose **Routing** in the sidebar. The page asks for the management key once per browser tab.

- **Decision tree**: the tree is the diagram. Add, change, and remove nodes in place. **Try a request** simulates a request against your *unsaved* edits and highlights the path it takes, then re-runs after every edit.
- **Accounts**: 5-hour and weekly usage, reset countdowns, and paid credit for each Claude and Codex OAuth account.
- **Clients**: client API keys that have made requests. Name one to use it in `client` conditions.
- **Activity**: the last 50 decisions. Click one to see its path in the tree.
- **Settings** and **Raw**: timezone, session pinning, poll interval, model routes, and the raw JSON.

Every edit is validated by the plugin before **Save** is enabled, because the host disables the plugin when it receives an invalid config. Saving writes `plugins.configs.lachieh-router` in `config.yaml` through the host's config API and takes effect without a restart.

## Configuration

The page edits this YAML. You can also write it by hand:

```yaml
plugins:
  enabled: true
  configs:
    lachieh-router:
      priority: 10
      timezone: Europe/London             # for hours/days; defaults to host local time
      sticky_ttl: 6h                      # idle time before a session's pin expires
      session_headers: [X-Claude-Code-Session-Id]
      usage: { interval: 5m }             # how often to poll Claude/Codex usage (min 1m)
      clients:                            # name -> SHA-256 of the client key (set these on the Clients tab)
        work-laptop: f450f062c6d2c6c8686321c9bed55927aaefb6ca38dbeb38c3b7289d77300a90

      routes:
        - name: plan-on-codex
          match: { model: "opus-plan" }
          provider: codex                 # skipped when the provider has no account
          model: gpt-5.5                  # client-facing model name; omit to keep the requested model

      tree:
        name: claude only
        if: { provider: claude }
        else: { builtin: true }           # other providers: host default
        then:
          name: work laptop
          if: { client: work-laptop }
          then: { pick: { accounts: ["claude-work*"] }, sticky: true }
          else:
            name: expiring allowance      # spend allowance that resets soon before it is lost
            pick:
              where: { weekly: { resets_within: 24h, used_below: 85 } }
              order: soonest_reset
            sticky: true
            else:
              name: personal
              pick:
                accounts: ["claude-personal*"]
                where: { five_hour: { used_below: 90 } }
              else:
                name: paid credit
                pick: { where: { credits: true } }
                else: { reject: "All Claude accounts are exhausted." }
```

### Tree nodes

Each node sets exactly one of:

| Node | Does |
|---|---|
| `if: <condition>` | Continues at `then` when the condition matches, otherwise at `else`. |
| `pick: {accounts, where, order, window}` | Picks an account from the ones the host offers. Continues at `else` when none qualify. |
| `delegate: round-robin \| fill-first` | Hands the choice to a built-in scheduler. |
| `reject: "message"` | Fails the request with `auth_unavailable`. |
| `builtin: true` | Uses the host's configured strategy. A missing `then` or `else` does the same. |

**Conditions** (`if`, and `match` on model routes) are ANDed globs: `model`, `provider`, `client` (a name from `clients`), `session`, `headers: {Name: glob}`, `hours: "09:00-17:30"` (may wrap midnight), and `days: [mon, …]`.

**Pick** options:

- `accounts`: ID globs in preference order. Leave it empty to allow any offered account.
- `where`: usage filters. `weekly` and `five_hour` each take `used_below`, `used_at_least` (percent), and `resets_within` (duration). `credits: true` requires paid extra usage to be available.
- `order`: `listed` (default), `soonest_reset`, or `least_used`, compared on `window` (`weekly` by default). Ties fall back to the `accounts` order.
- `sticky: true` keeps a session on the account it was given for as long as that account still qualifies at this node. A usage threshold therefore moves a session off an account that is running out.

**The tree runs for every provider.** A pick whose `where` needs usage data never qualifies an account without usage, such as an API-key provider. Root the tree at `if: {provider: claude}` (or `codex`) with `else: {builtin: true}`, as in the example, or a chain of picks ends at its reject. Simulate a request for each provider before saving.

### Usage data

Every `usage.interval`, the plugin reads each enabled Claude and Codex OAuth credential through the host and calls the same usage endpoints the panel's Quota page uses (`api.anthropic.com/api/oauth/usage` and `chatgpt.com/backend-api/wham/usage`). Some things to know:

- Picks read only the cached readings, so a slow usage endpoint never delays a request.
- A reading older than three intervals is ignored. An account without a recent reading fails every usage condition.
- These endpoints are undocumented and can change. When a fetch fails, the Accounts tab shows the error.
- Access tokens stay in plugin memory only for the duration of each fetch. They are never logged or stored.
- The host only offers accounts that are currently usable, so accounts in cooldown are excluded before the tree runs.

When this plugin handles a pick, the host's own session affinity does not run, so use `sticky`. Pins, observed client keys, and recent decisions live in memory. They survive config changes but not a host restart.

### Session IDs and account IDs

The session comes from host metadata (`canonical_session_id`, `execution_session_id`, `derived_session_id`), then from `session_headers`. The host prefixes session IDs that come from headers, for example `header:abc123`.

Account globs match credential IDs. For OAuth credentials that's the file name. API-key credentials get hashed IDs such as `openai-compatibility:mock:fc8e4dcef674`. The Accounts tab lists every ID the plugin has seen.

### Management API

All routes require the management key (`Authorization: Bearer …`) and live under `/v0/management/plugins/lachieh-router/`:

| Route | |
|---|---|
| `GET state` | Seen accounts and clients, pins, recent decisions with paths, cached usage. |
| `POST validate` | Body: a config object. Returns `{ok, errors}`. |
| `POST simulate` | Body: `{config?, input: {model, provider, client, session, headers, at, candidates?}}`. Returns the result and path. Without `config` it uses the live rules. |
| `POST usage/refresh` | Polls usage now. |

To read and write the config itself, use the host's `GET`/`PUT /v8/management/config/plugins/configs/lachieh-router`.

## Install

Add this repository's store to CLIProxyAPI, then install **Lachieh Router** from the plugin store:

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/lachieh/cliproxyapi-plugins/main/registry.json"
```

## Development

From the repository root:

```bash
go test ./plugins/router/...                                              # rule engine and usage parser tests
CGO_ENABLED=1 go build -buildmode=c-shared -o dist/lachieh-router.so ./plugins/router
```

Copy the library into `<plugins.dir>/<goos>/<goarch>/` and restart CLIProxyAPI, or reinstall through the store. Releases are cut with a `router/v<version>` tag; see the repository README.
