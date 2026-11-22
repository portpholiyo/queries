# queries

A small, production-minded Go service that lets website visitors send a question (text plus up to three images), get a private tracking ID, and read the owner's replies, with the owner answering from inside a Slack thread. Built for [portpholiyo.com](https://portpholiyo.com).

> This is the public showcase of a privately run project. Some production details live only in the private repo.

**Start here:** [Architecture](docs/architecture.md) · [Design decisions (ADRs)](docs/adr/README.md)

## Highlights
- **Never loses a query:** the query is stored first; a worker delivers it to Slack with leases, exponential backoff and idempotent retries, so a Slack outage only delays delivery.
- **No accounts, no leaks:** the unguessable query ID is the only credential for reading a thread, and it is never logged.
- **Safe uploads:** images are identified by content (never by filename or declared type), SVG is rejected, JPEG metadata (EXIF/XMP, including GPS) is stripped, and files are served with a locked-down CSP.
- **Two-way Slack sync without a public endpoint:** inbound events over an outbound WebSocket, plus a periodic reconcile for anything missed while the service was down.
- **Layered abuse controls:** CAPTCHA, per-IP rate limits, a daily cap, bounded concurrent uploads, follow-up limits, HMAC-hashed IPs.
- **Testable:** Slack, CAPTCHA and the store sit behind interfaces; unit tests need no network. Store tests run against a real PostgreSQL when `TEST_DB_PORT` is set.

## API
| Method | Path | Purpose |
|---|---|---|
| POST | `/api/query` | Create a query (multipart: text, images, CAPTCHA token) |
| GET | `/api/query/{id}` | Read the thread (query, replies, file URLs) |
| POST | `/api/query/{id}/replies` | Visitor follow-up |
| GET | `/api/query/{id}/files/{file}` | Serve an attached image |
| GET | `/healthz` | Liveness |

## Develop
```
go vet ./... && go test -race ./...
docker build -t queries .
```
Configuration is environment-based; see `internal/config/config.go` for every variable and default. Secrets are read from files (`*_FILE` variables), never from plain environment variables.

## License
MIT, see [LICENSE](LICENSE).
