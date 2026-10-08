<img src="assets/icon.svg" width="64" height="64" alt="">

# cliproxyapi-plugin-lachieh

A personal [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin, `lachieh-router`, that routes sessions by my preferences:

- **`routes`** (`model.route`): before provider/account selection, send matching requests to a provider and optionally rewrite the model.
- **`accounts`** (`scheduler.pick`): choose which credential serves a request from an ordered preference list, and pin sessions to the account they started on.

Rules match on requested model, provider, request headers, session ID, time of day, and day of week. For each list, the first matching rule wins.

## Configuration

```yaml
plugins:
  enabled: true
  configs:
    lachieh-router:
      priority: 10
      timezone: Australia/Sydney          # for hours/days; defaults to host local time
      sticky_ttl: 6h                      # idle time before a session's pin expires
      session_headers: [X-Claude-Code-Session-Id]   # fallback if the host has no session ID

      routes:
        - name: plan-on-codex
          match: { model: "opus-plan" }
          provider: codex                 # host provider key; rule is skipped if it has no auth
          model: gpt-5.5                  # client-facing model name/alias; omit to keep the requested model
        - name: cheap-profile
          match: { headers: { X-Route-Profile: cheap } }
          provider: gemini

      accounts:
        - name: work-hours
          match:
            model: "claude-*"
            headers: { User-Agent: "claude-cli/*" }
            hours: "09:00-17:30"          # [start, end); may wrap midnight, e.g. 22:00-06:00
            days: [mon, tue, wed, thu, fri]
          prefer: ["claude-work@*", "claude-personal@*"]
          sticky: true
        - name: default-claude
          match: { provider: claude }
          prefer: ["claude-personal@*", "claude-work@*"]
          sticky: true
          fallback: round-robin           # builtin (default) | round-robin | fill-first | reject
```

Globs are case-insensitive. `*` matches any run of characters, including `/`, and `?` matches a single character. A `headers` condition matches when any value of that header matches.

### How account picking works

1. The host only offers candidates that are currently usable. Accounts in cooldown or out of quota are already excluded, so preference order falls through to the next account automatically.
2. If the rule is `sticky` and the session already has a pin that is still a candidate, the pinned account is used. This keeps prompt caches warm even after a more-preferred account recovers.
3. Otherwise the first candidate matching the first `prefer` glob wins, then the second glob, and so on. Sticky rules pin that choice.
4. If no preferred account is available, `fallback` decides: hand the request back to the host's configured strategy (`builtin`), delegate to a named built-in scheduler, or `reject` with `auth_unavailable`.

When this plugin handles a pick, the host's own session affinity does not run. Use `sticky: true` to get that behavior.

Pins and recent decisions live in memory. They survive config changes but are lost when the host restarts.

### Session IDs

The session comes from host metadata (`canonical_session_id`, `execution_session_id`, `derived_session_id`), then from `session_headers`. The host prefixes session IDs derived from headers, for example `header:abc123`, so write `session` globs like `"header:*"` or `"*abc*"`.

### Finding account IDs

`prefer` globs match credential IDs. For OAuth/file credentials that's the file name (see `GET /v0/management/auth-files`). API-key credentials get hashed IDs such as `openai-compatibility:mock:fc8e4dcef674`. The plugin records every account the host has offered it, plus current pins and the last 50 decisions:

```bash
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/lachieh-router/state
```

### Invalid config

Unknown fields and bad values are rejected. The host logs the error and marks the plugin `registered: false` until the config is fixed, and requests use the host's built-in routing in the meantime. Check `GET /v0/management/plugins` after editing.

## Development

```bash
make test                        # rule engine tests
make build                       # dist/lachieh-router.{dylib,so}
make install PLUGINS_DIR=...     # copy into <plugins.dir>/<goos>/<goarch>/
make package VERSION=v0.1.0      # dist/lachieh-router_0.1.0_<goos>_<goarch>.zip
```

Restart CLIProxyAPI, or reinstall through the store, after replacing the library.

## Private publishing

Pushing a `v*` tag runs `.github/workflows/release.yml`. It builds darwin/linux × amd64/arm64 zips and `checksums.txt`, then publishes them as a GitHub release.

The repo is private, so give CLIProxyAPI a read-only fine-grained token (Contents: read on this repo) and point the store at this repo's `registry.json`:

```yaml
plugins:
  store-sources:
    - "https://raw.githubusercontent.com/lachieh/cliproxyapi-plugin-lachieh/main/registry.json"
  store-auth:
    - match: "https://raw.githubusercontent.com/lachieh/cliproxyapi-plugin-lachieh/"
      apply-to: ["registry"]
      type: "github-token"
      token-env: "LACHIEH_PLUGIN_GITHUB_TOKEN"
    - match: "https://api.github.com/repos/lachieh/cliproxyapi-plugin-lachieh/"
      apply-to: ["metadata", "artifact"]
      type: "github-token"
      token-env: "LACHIEH_PLUGIN_GITHUB_TOKEN"
```

Then install or update with `POST /v0/management/plugin-store/lachieh-router/install`.
