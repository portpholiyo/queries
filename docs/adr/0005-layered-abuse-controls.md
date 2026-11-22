# 0005. Layer abuse controls instead of relying on one

- Status: Accepted
- Date: 2026-11-22

## Context
An open form that stores data and notifies a person is a target for spam, floods and resource exhaustion. Any single control can fail or be bypassed.

## Options considered
1. A CAPTCHA only: stops simple bots but not a determined flood, and the provider can be unavailable.
2. Rate limiting only: easy to evade with many addresses.
3. Several independent layers, each with a clear failure mode.

## Decision
Option 3:
- Server-side CAPTCHA verification that fails closed.
- Per-IP limiters on submit, follow-up and read endpoints.
- A global daily cap on new queries and on follow-ups, plus per-thread follow-up limits.
- A semaphore that bounds concurrent submissions, and request body caps, so memory stays bounded.
- IPs stored only as an HMAC with a secret salt, which allows abuse analysis without keeping addresses.

## Consequences
- A flood is bounded in cost and in how much it can notify the owner.
- Legitimate users can be refused at the caps; limits are configurable and surfaced as clear errors.
- If the CAPTCHA provider is down, submissions are refused rather than accepted unchecked.
- Per-IP limits depend on the proxy supplying the real client address through a configured header.
