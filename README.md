# relay-ping

`relay-ping` is a small interoperability test tool for [chatmail](https://chatmail.at/) relays. It provisions accounts via `POST /new`, exercises SMTP/IMAP, SecureJoin initiation, throughput, and optional **N×N latency matrices** across many servers—with a local web UI and **WebXDC** packaging for Delta Chat.

---

## Build

Requires **Go 1.26+** (see `go.mod`).

| Target | Description |
|--------|-------------|
| `make build` | Produce `bin/relay-ping`. |
| `make build-webxdc` | Zip `web/` into `bin/relay-ping.xdc` (offline viewer; file picker). Flips `WEBXDC_BUILD_MODE` in `web/static/app.js` during the zip step and restores it afterward. |
| `make build-webxdc-with-results` | Same as `build-webxdc`, but embeds a matrix export: set **`RUN_EXPORT=path/to/run.json.gz`** (copied to `web/static/bundled-run.json.gz` for one load on open). Writes `bin/relay-ping-with-results.xdc`. |
| `make clean` | Remove `bin/`. |

---

## CLI overview

Run `./bin/relay-ping -help` for flag text from `cmd/relay-ping/main.go`.

### Global flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-domain` | `https://nine.testrun.org/` | Chatmail base URL; used to derive `/new` if `-endpoint` is empty and for default SMTP/IMAP hosts. |
| `-endpoint` | *(empty)* | Full provisioning URL (overrides `EndpointFromDomain`). |
| `-test` | `connectivity` | Test mode: see [Tests](#tests). |
| `-timeout` | `20s` | **Per-process** `context` timeout (raise for long `latency_matrix` runs, e.g. `-timeout 3h`). |
| `-log-file` | `relay-ping.log` | Verbose log file; use **`-log-file -`** to log only to stdout (no file). |
| `-color` | `always` | `auto` / `always` / `never` for ANSI colors. |
| `-v` | off | Verbose: HTTP and general progress. |
| `-vv` | off | Also SMTP + IMAP line logs. |
| `-vvv` | off | Also full HTTP bodies and low-level “wire” trace. |

**Note:** Verbose logging only activates when at least one of `-v` / `-vv` / `-vvv` is set; until then, detail writers are discarded.

### Tests (`-test`)

| Value | What it does |
|--------|----------------|
| `connectivity` | `POST /new` → SMTP STARTTLS + PLAIN auth → IMAP TLS login (`internal/check/smtpcheck`, `internal/check/imapcheck`). |
| `securejoin-init` | Two accounts, OpenPGP keys, invite URI, joiner sends phase‑1 over SMTP (`internal/check/securejoininit`). |
| `throughput` | Runs SecureJoin pre-check, then sends `-count` encrypted messages with `-workers` parallelism; measures delivery via IMAP (`internal/check/throughput`). Uses `./tmp` for keys and payloads. |
| `latency_matrix` | Pairwise latency matrix across **≥2** hosts (`internal/check/latencymatrix`). See below. |
| `test-webserver` | Static preview only: serves `web/` over HTTP and redirects `/` → `/?webxdc=1`; tries `xdg-open` (`cmd/relay-ping/standalone.go`). **Does not** run the matrix. |

### Latency matrix flags (`-test latency_matrix`)

| Flag | Default | Meaning |
|------|---------|---------|
| `-servers` | *(empty)* | Comma-separated hostnames (optional shorthand: single `@servers.txt` reads that file). |
| `-servers-file` | *(empty)* | One host per line; `#` comments allowed. |
| `-worker` | `4` | Concurrent pair workers (each holds SMTP+IMAP work); **clamped to 16** inside `latencymatrix.Run`. |
| `-webserver` | `false` | Enable live UI + WebSocket progress (`cmd/relay-ping/webserver.go`). |
| `-web-addr` | `127.0.0.1:8787` | Listen address for `-webserver`. |
| `-export` | *(empty)* | After the run, write **gzip JSON** via `latencymatrix.ExportRun` (embedded logs; see [Export format](#latency-matrix-export-jsongz)). |

Without `-webserver`, progress prints on stdout with account/status lines on stderr so the terminal layout stays readable.

### Throughput flags (`-test throughput`)

| Flag | Default | Meaning |
|------|---------|---------|
| `-count` | `20` | Messages to send. |
| `-workers` | `8` | Parallel send workers (capped vs count inside the package). |

### Connectivity overrides

| Flag | Meaning |
|------|---------|
| `-smtp` | Force SMTP `host:port`. |
| `-imap` | Force IMAP `host:port`. |

---

## Tests (behavior)

### Connectivity

1. Resolve provisioning URL: `https://<host>/new` unless `-endpoint` is set (`internal/chatmail.EndpointFromDomain`).
2. `POST` with empty body; JSON response decoded into `Account` (`internal/chatmail`).
3. If SMTP/IMAP are missing, defaults: **`<hostname>:587`** and **`<hostname>:993`** (`DefaultMailAddrsFromDomain`).
4. **SMTP:** STARTTLS to `host:port`, PLAIN auth, QUIT (`internal/check/smtpcheck`).
5. **IMAP:** TLS dial, LOGIN, LOGOUT (`internal/check/imapcheck`).

### SecureJoin initiation

Creates inviter + joiner accounts, generates keys with gopenpgp, builds a SecureJoin invite (`https://i.delta.chat/#…`), joiner sends the phase‑1 SMTP message. Returns OK/FAIL (`internal/check/securejoininit`).

### Throughput

Prepares OpenPGP material under `./tmp`, reuses or creates key files, builds one encrypted raw packet, runs the same SecureJoin path as `securejoin-init` as a **pre-check**, then floods encrypted mail and measures delivery, latency (avg/p95), errors, and bandwidth (`internal/check/throughput`).

### Latency matrix

For each listed server, provisions accounts (two per server for intra- and cross-server pairs), runs directed pair probes, fills an **N×N** matrix with per-cell status/latency/logs, and optionally computes aggregate **RunStatus** (good/mid/slow/bad buckets). Parallelism is bounded by `-worker` (max 16).

### `test-webserver`

Development helper to open the static WebXDC-oriented UI from disk—not coupled to a running matrix.

---

## Web UI and WebXDC

### Matrix server (`-test latency_matrix -webserver`)

`cmd/relay-ping/webserver.go` serves:

- `/` — matrix HTML
- `/static/*` — assets
- `/ws` — WebSocket progress + final result JSON
- `/pair-log`, `/pair-log-stream` — pair log helpers for the UI

Run from the **repository root** so the `web/` directory exists.

### Standalone static server (`-test test-webserver`)

`cmd/relay-ping/standalone.go` serves `web/` without WebSockets—useful for checking the offline/WebXDC bundle behavior (`/?webxdc=1`).

### WebXDC packaging (`make build-webxdc`)

Produces a `.xdc` zip (`web/manifest.toml`, `index.html`, `static/`). In bundle mode the UI uses file import instead of WebSockets unless Delta Chat injects APIs.

---

## Latency matrix export (`.json.gz`)

`internal/check/latencymatrix/export.go`:

- **`ExportRun`** writes **one gzip-compressed JSON** document.
- Embeds **`run.log`** as `runLogs` and each pair log file into matrix cells as `logs`; clears `logPath` fields in the export.
- **`export_test.go`** covers **`ParseLogLine`** / **`ParseLogBytes`** for bracketed timestamp lines used in logs.

Consumers (including `web/static/app.js`) may gzip-decompress and parse JSON; the payload matches `latencymatrix.Result` (servers, matrix, status, runLogs, …).

---

## Package map (`internal/`)

| Package | Role |
|---------|------|
| **`internal/chatmail`** | `EndpointFromDomain`, `DefaultMailAddrsFromDomain`, `FetchAccount`: POST `/new`, flexible JSON field names (`username`/`user`/`email`, `smtp`/`smtp_server`/host+port, etc.). |
| **`internal/check/smtpcheck`** | SMTP STARTTLS + PLAIN login smoke test; respects context cancel. |
| **`internal/check/imapcheck`** | IMAP TLS login smoke test; respects context cancel. |
| **`internal/check/securejoininit`** | SecureJoin phase‑1 path for two fresh accounts. |
| **`internal/check/throughput`** | SecureJoin pre-check + encrypted flood + IMAP delivery metrics; artifacts under `./tmp`. |
| **`internal/check/latencymatrix`** | Matrix orchestration, logging dir, `ExportRun` re-exported through `export.go`. |

Vendor trees under `test/` are reference copies of upstream libraries—not part of the shipped binary.

---

## Public relay list for matrices

The canonical list of hosts advertised at [chatmail.at/relays](https://chatmail.at/relays) can be refreshed into `servers.txt`:

```bash
./scripts/fetch-chatmail-relays.sh > servers.txt
```

(Comments at the top of `servers.txt` are ignored by the CLI parser.)

---

## Examples

```bash
# Connectivity (default test)
./bin/relay-ping -domain https://nine.testrun.org/

# SecureJoin initiation
./bin/relay-ping -test securejoin-init -domain https://nine.testrun.org/

# Throughput
./bin/relay-ping -test throughput -domain https://nine.testrun.org/ -count 20 -workers 8

# Latency matrix (file), export, long timeout
./bin/relay-ping -test latency_matrix -servers-file servers.txt \
  -export /tmp/matrix.json.gz -timeout 3h -color never -log-file -

# Matrix + browser UI
./bin/relay-ping -test latency_matrix -servers-file servers.txt -webserver -web-addr 127.0.0.1:8787

# Verbose to stdout only
./bin/relay-ping -test connectivity -domain https://nine.testrun.org/ -v -log-file -
```

---

## Plan / follow-ups

- Richer `/new` schema handling if providers diverge.
- SecureJoin phases beyond initiation.
- Optional STARTTLS/TLS flags for SMTP/IMAP.
- Machine-readable CLI output (`--json`) for scripting.
- Integration tests with mock `/new` and fake mail servers.
