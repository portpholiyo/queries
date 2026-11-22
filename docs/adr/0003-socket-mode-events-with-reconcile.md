# 0003. Sync replies with Socket Mode events plus a periodic reconcile

- Status: Accepted
- Date: 2026-11-22

## Context
The owner answers inside Slack and the visitor must see the answer on the tracking page. The service sits behind a reverse proxy and should not need to expose a public callback URL for a third party to call.

## Options considered
1. Slack HTTP events to a public endpoint: requires exposing and verifying a callback route.
2. Polling every thread constantly: wasteful and slow to react.
3. Socket Mode (an outbound WebSocket) for events, plus a low-frequency reconcile as a safety net.

## Decision
Option 3. An event only means "this thread changed": the worker re-reads that thread and stores any new owner messages. A unique constraint on the Slack timestamp makes processing idempotent. Every five minutes the worker also re-reads threads from the last seven days, which catches events missed while the service was down. Only messages from the configured owner are accepted, and unsupported attachments are skipped with a note posted in the thread.

## Consequences
- No inbound public endpoint for Slack; the connection is outbound only.
- Missed or duplicated events do not matter: the stored thread converges to the Slack thread.
- Replies older than the reconcile window are only picked up through events; the window is configurable.
