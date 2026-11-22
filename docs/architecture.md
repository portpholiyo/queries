# Architecture

Current state of the `queries` service. Update this file in the same change as any code change that alters components, flows, limits or data (CI checks that every path mentioned here exists).

## Context
```mermaid
flowchart LR
  V[Visitor] --> P[Reverse proxy]
  P --> Q[queries service]
  Q <--> D[(PostgreSQL)]
  Q <--> S[Slack]
  O[Owner] -.-> S
```
Visitors reach the service only through a reverse proxy; the service binds to loopback only. It posts queries to Slack and reads replies back over one outbound WebSocket, so it needs no public callback endpoint. The owner answers from inside Slack.

## Components
| Path | Responsibility |
|---|---|
| `cmd/server/main.go` | Wiring (config, store, Slack clients, worker, HTTP server), graceful shutdown, `healthcheck` subcommand for the container |
| `internal/config/config.go` | Environment variables, limits and defaults; secrets read from `*_FILE` paths |
| `internal/server/server.go` | HTTP routes, multipart parsing and validation, CAPTCHA check, limits, CORS allow-list, request log |
| `internal/server/ratelimit.go` | Per-IP limiter |
| `internal/images/images.go` | Content sniffing, SVG rejection, JPEG metadata stripping |
| `internal/queryid/queryid.go` | Query ID generation and normalisation |
| `internal/captcha/captcha.go` | Server-side CAPTCHA token verification (fails closed) |
| `internal/slackx/slackx.go` | Thin Slack client: post, upload, read thread, download, rate-limit errors, escaping |
| `internal/worker/worker.go` | Outbox delivery to Slack with backoff, and inbound sync (events plus periodic reconcile) |
| `internal/store/store.go` | pgx pool, embedded migrations, all SQL, outbox claiming, follow-up limits |
| `Dockerfile` | Multi-stage build, distroless, non-root |

## Creating a query and delivering it
```mermaid
sequenceDiagram
  participant B as Browser
  participant S as Server
  participant D as PostgreSQL
  participant W as Worker
  participant L as Slack
  B->>S: POST /api/query (text, images, CAPTCHA token)
  S->>S: per-IP limit, concurrency slot, validate, sniff images
  S->>S: verify CAPTCHA (fail closed)
  S->>D: daily cap check, then store query, files and outbox state
  S-->>B: query ID
  S->>W: wake
  loop until delivered
    W->>D: claim next pending query (lease, SKIP LOCKED)
    W->>L: post root message
    W->>D: save thread timestamp immediately
    W->>L: upload images into the thread
    W->>D: mark done (or record failure and back off)
  end
```
The answer to the visitor does not wait for Slack, so a Slack outage never loses or blocks a query ([ADR-0002](adr/0002-transactional-outbox-for-slack-delivery.md)).

## Replies coming back
```mermaid
sequenceDiagram
  participant O as Owner
  participant L as Slack
  participant W as Worker
  participant D as PostgreSQL
  participant B as Browser
  O->>L: reply in the thread
  L-->>W: event (Socket Mode)
  W->>L: re-read that thread
  W->>D: store new owner messages (unique Slack timestamp makes it idempotent)
  loop every 5 minutes
    W->>L: re-read threads from the last 7 days
  end
  B->>D: GET /api/query/{id} reads the thread
```
Events only trigger a re-read of that thread; messages not yet stored are added, and only messages from the configured owner are accepted ([ADR-0003](adr/0003-socket-mode-events-with-reconcile.md)).

## Data
```mermaid
erDiagram
  direction LR
  QUERIES ||--o{ REPLIES : has
  QUERIES ||--o{ FILES : has
  REPLIES ||--o{ FILES : has
  QUERIES {
    text id PK
    text ip_hash
    text body
    text slack_ts
    bool slack_done
    int slack_attempts
    timestamptz next_attempt_at
  }
  REPLIES {
    bigint id PK
    text query_id FK
    text sender
    text body
    text slack_ts
    bool slack_done
  }
  FILES {
    uuid id PK
    text query_id FK
    bigint reply_id FK
    text mime
    bytea data
    bool slack_sent
  }
```
Visitor follow-ups share the replies table with the owner's messages (`sender` distinguishes them) and use the same outbox columns. Migrations are embedded in the binary and applied on boot under an advisory lock, so concurrent starts are safe. Images are stored in the database, which keeps backups to a single database dump.

## Limits (defaults in `internal/config/config.go`)
2000-character message, 3 images of at most 5 MiB, a daily cap on new queries, and for follow-ups: 30 per thread, 5 per 10 minutes, a daily cap across all threads. Per-IP limiters guard submit, follow-up and read endpoints, and a semaphore bounds concurrent submissions so memory stays bounded ([ADR-0005](adr/0005-layered-abuse-controls.md)).

## Security and privacy
- Images are validated by content; SVG is rejected; JPEG EXIF/XMP is stripped ([ADR-0004](adr/0004-validate-images-by-content.md)).
- Served files carry `X-Content-Type-Options: nosniff`, a `default-src 'none'; sandbox` CSP and `no-store` caching on API responses.
- The query ID is a bearer capability and is never logged ([ADR-0001](adr/0001-query-id-as-bearer-capability.md)).
- IPs are stored only as an HMAC with a secret salt; message text is rendered with `textContent` on the site.
- Secrets only through files, never plain environment variables or the repository; the container runs as non-root on a distroless image.

## Testing strategy
Slack, CAPTCHA and the store are interfaces; tests use fakes, so `go test -race ./...` needs no network or secrets. Store tests are integration tests against a real PostgreSQL and skip unless `TEST_DB_PORT` is set ([ADR-0006](adr/0006-interfaces-and-real-database-tests.md)).
