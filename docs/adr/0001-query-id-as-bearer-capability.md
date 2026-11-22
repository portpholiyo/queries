# 0001. Use the query ID as a bearer capability instead of accounts

- Status: Accepted
- Date: 2026-11-22

## Context
Visitors should be able to ask a question and come back to read the answer without signing up. Asking for an email address adds friction, stores personal data and creates something to protect and to send mail from.

## Options considered
1. Accounts or email verification: heavy for a one-off question and creates personal data to secure.
2. Cookie or session only: the visitor loses access on another device or after clearing storage.
3. An unguessable query ID that is the only credential for reading the thread.

## Decision
Option 3. IDs look like `Q-XXXXX-XXXXX`: 50 random bits in Crockford base32, which avoids look-alike characters and is normalised on input. Read endpoints are rate limited per IP so guessing is impractical, and the ID is never written to logs.

## Consequences
- No personal data is collected, and the visitor can return from any device with the ID.
- Anyone who has the ID can read the thread, so it must be treated like a secret: it is never logged and lookups return a uniform not-found.
- Losing the ID means losing access; this is an accepted trade-off.
