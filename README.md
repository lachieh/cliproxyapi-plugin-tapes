# cliproxyapi-plugins

Native plugins for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), published as one plugin store.

| Plugin | Store ID | What it does |
| --- | --- | --- |
| [tapes](plugins/tapes) | `tapes` | Records every proxied request and response as sessions in [tapes](https://tapes.dev). |
| [Lachieh Router](plugins/router) | `lachieh-router` | Routes requests to providers and accounts by rules and live account usage, edited on a Routing page in the management panel. |

## Install

Turn on plugins and add this repository's store in CLIProxyAPI's `config.yaml`:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/lachieh/cliproxyapi-plugins/main/registry.json"
```

The plugins then appear in the management panel's plugin store. Each plugin's README covers its settings.

## Layout

| Path | Purpose |
| --- | --- |
| `plugins/<name>` | One plugin: a `main` package, its `store.json` (store listing metadata) and its README. |
| `internal/cabi` | The C ABI every plugin shares: the exports CLIProxyAPI loads, response envelopes, panic recovery and host callbacks. |
| `scripts/package` | Zips a built library the way the store installs it. |
| `scripts/registry` | Records a release in `registry.json`. |

## Develop

```sh
go vet ./...
go test -race ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o dist/tapes.so ./plugins/tapes
```

## Release

Push a tag named `<plugin directory>/v<version>`, for example `tapes/v0.4.0` or `router/v0.3.0`. The release workflow:

1. Builds that plugin for linux (amd64, arm64), darwin (amd64, arm64) and windows (amd64). Linux builds run on Debian bookworm to match the official CLIProxyAPI image.
2. Publishes the zips and `checksums.txt` as a GitHub release.
3. Records the release in `registry.json` on `main`.

`registry.json` uses the store's direct installs (schema 2), with download URLs and checksums for each platform. The store's other install type always takes a repository's latest release, so it can't serve more than one plugin per repository. Don't edit plugin versions in `registry.json` by hand; edit a plugin's `store.json` to change its listing.

## License

MIT
