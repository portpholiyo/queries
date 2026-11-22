# 0002. Deliver to Slack through a transactional outbox

- Status: Accepted
- Date: 2026-11-22

## Context
Delivering a query to Slack is a network call to a third party that can be slow, rate limited or down. The visitor should get an ID immediately, and a query must never be lost or posted twice, including across restarts.

## Options considered
1. Call Slack inside the request: the visitor waits on Slack, and a failure either loses the query or surfaces as an error.
2. An in-memory queue: lost on restart.
3. Store the query first, then deliver from a database-backed outbox processed by a worker.

## Decision
Option 3. The query and its images are stored in one transaction and the request returns. A worker claims the next pending row with a lease (`FOR UPDATE SKIP LOCKED`), posts the root message, saves the thread timestamp immediately, uploads images one by one marking each sent, then marks the row done. Failures are recorded with exponential backoff that respects Slack's rate limits. Visitor follow-ups use the same pattern.

## Consequences
- A Slack outage delays delivery but never loses a query; restarts resume where they stopped.
- Saving the thread timestamp before uploading images makes retries idempotent: no duplicate root posts.
- Leases make it safe to run more than one worker instance later.
- Delivery is at-least-once for individual image uploads in a narrow crash window; this is acceptable for this use.
