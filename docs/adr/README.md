# Architecture Decision Records

One file per significant decision: context, options, decision, consequences. Add a new ADR in the same change as the decision; never rewrite an accepted one, supersede it.

| # | Decision | Status |
|---|---|---|
| [0001](0001-query-id-as-bearer-capability.md) | Use the query ID as a bearer capability instead of accounts | Accepted |
| [0002](0002-transactional-outbox-for-slack-delivery.md) | Deliver to Slack through a transactional outbox | Accepted |
| [0003](0003-socket-mode-events-with-reconcile.md) | Sync replies with Socket Mode events plus a periodic reconcile | Accepted |
| [0004](0004-validate-images-by-content.md) | Validate images by content, reject SVG, strip metadata | Accepted |
| [0005](0005-layered-abuse-controls.md) | Layer abuse controls instead of relying on one | Accepted |
| [0006](0006-interfaces-and-real-database-tests.md) | Interfaces for external services, real database for store tests | Accepted |

Template: [template.md](template.md)
