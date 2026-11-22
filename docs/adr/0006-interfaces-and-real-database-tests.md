# 0006. Interfaces for external services, real database for store tests

- Status: Accepted
- Date: 2026-11-22

## Context
The service depends on Slack, a CAPTCHA provider and PostgreSQL. Tests that call Slack are flaky and notify a real person; mocking the database hides SQL mistakes that only a real engine reveals.

## Decision
- Slack client, CAPTCHA verifier, the server's store and the worker's store are small interfaces defined where they are consumed and injected in `main`. Tests use fakes and exercise the real HTTP handlers, limits, retry logic and idempotency rules.
- Store tests run against a real PostgreSQL and skip unless `TEST_DB_PORT` is set, so the default suite stays fast and offline while SQL behaviour (claiming, leases, unique constraints) is verified for real when wanted.

## Consequences
- `go test -race ./...` runs offline with no secrets.
- Fakes must be kept honest; the real-database tests cover the parts fakes cannot.
- CI runs the offline suite; the database tests are run locally against a throwaway container.
