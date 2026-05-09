# relay-ping

`relay-ping` is a small interoperability test tool for chatmail servers.

It creates a fresh chatmail account via `POST /new`, then verifies that the same credentials can authenticate on both SMTP and IMAP.

## Tests

### Test 1: Connectivity

1. Send `POST` to `https://nine.testrun.org/new` (or another endpoint).
2. Parse returned account data (`username`/`password`, and SMTP/IMAP addresses if provided).
3. Connect to SMTP and run authenticated login.
4. Connect to IMAP and run authenticated login.

If both logins succeed, the server is considered interoperable for this basic auth flow.

### Test 2: SecureJoin initiation

1. Create inviter account (`POST /new`).
2. Create joiner account (`POST /new`).
3. Generate inviter OpenPGP key and fingerprint using `gopenpgp`.
4. Build SecureJoin invite URI (`https://i.delta.chat/#...`).
5. Joiner sends SecureJoin phase-1 message (`vc-request`) to inviter via authenticated SMTP.

This validates account creation + key setup + protocol kickoff. It is designed as the base for adding full phase 2/3/4 SecureJoin tests.

### Test 3: Throughput

Pipeline:

1. On first run, create and persist OpenPGP keypairs in `./tmp`.
2. Create one encrypted raw packet and store it in `./tmp/encrypted-raw-packet.asc`.
3. Run SecureJoin init pre-check.
4. Send N encrypted raw messages and wait for each delivery via IMAP.
5. Report:
   - Messages per second (MPS)
   - Latency (avg + p95)
   - Error rate
   - Bandwidth out/in (bytes per second)

## Project structure

- `cmd/relay-ping`: CLI entrypoint and orchestration.
- `internal/chatmail`: fetches and parses account provisioning response.
- `internal/check/smtpcheck`: SMTP connectivity/auth check.
- `internal/check/imapcheck`: IMAP connectivity/auth check.
- `test/`: upstream vendor test libraries/reference code (not part of the runtime tool).

Only the needed client usage was brought into the main source (`internal/check/*` and `internal/chatmail/*`). The full test libraries remain in `test/` as references.

## Build

```bash
make build
```

Binary path: `bin/relay-ping`

## Run

```bash
./bin/relay-ping -test connectivity -domain https://nine.testrun.org/
```

If endpoint response does not include SMTP/IMAP addresses, the tool falls back to:
- SMTP: `<domain>:587`
- IMAP: `<domain>:993`

You can still override addresses manually:

```bash
./bin/relay-ping -smtp mail.example.org:587 -imap mail.example.org:993
```

Verbose logging is enabled by default to stdout and `relay-ping.log`.  
Use `-log-file -` to log only to stdout.

Run SecureJoin initiation test:

```bash
./bin/relay-ping -test securejoin-init -domain https://nine.testrun.org/
```

Run throughput test:

```bash
./bin/relay-ping -test throughput -domain https://nine.testrun.org/ -count 20
```

Verbosity controls logs:
- default: summary only
- `-v`: high-level progress
- `-vv`: SMTP/IMAP protocol logs
- `-vvv`: full trace including HTTP bodies

## Plan

- Add richer response parsing once exact `/new` schema is confirmed.
- Implement SecureJoin phase 2/3/4 verification (auth-required, request-with-auth, contact-confirm).
- Add STARTTLS/TLS mode flags for SMTP and IMAP.
- Add machine-readable output (`--json`) for CI pipelines.
- Add integration tests with a mock `/new` endpoint and fake mail servers.
