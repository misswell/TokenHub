# DOP native single-resource safety extension

The independent `cmd/tokenhub-dop` entry enables this extension. The original entry and source files remain unchanged. The DOP entry applies the quota read and user deletion optimizations described below.

On the DOP entry, ordinary policy lists read quota usage in batches from one database snapshot; a read failure returns a sanitized 500 instead of incomplete usage. DOP authentication and ordinary user deletion select the earliest user for a static admin token on each request without a permanent cache and fail closed on database errors. Other native routes retain their original authentication. The DOP entry creates an index on admin user creation time at startup.

On the DOP entrypoint, `GET /api/admin/resources/quota-policies?id={id}` uses native authentication and authorization, then queries the exact policy before applying visibility, usage enrichment, and response redaction. Existing callers do not need the DOP header or a different URL. An unknown or inaccessible ID returns an empty `data` array. Omitting `id` or passing an empty value retains the full visible list. With `X-DOP-Operation: true`, the same ID filter applies while the catalog continues to omit usage calculations and require a platform administrator. The original `cmd/tokenhub` entrypoint retains its original list behavior, including when `id` is supplied.

A platform administrator must authenticate with the native Admin Bearer Token. `GET /api/admin/dop/capabilities` advertises `native_single_receipts`, `managed_single_users`, and version 1. Opted-in writes require a stable `Idempotency-Key` (1–128 characters); changing a request under the same identity is rejected. Native handlers execute transactionally with an encrypted receipt; no HTTP call to a one-item batch endpoint occurs. Replay preserves the created identity or Key secret, within the existing 24-hour receipt window. DOP must persist its encrypted result and continue the original operation after an uncertain response.

## Opted-in native paths

| Method/path | Extension behavior |
| --- | --- |
| `POST /api/admin/users` | Create an OAuth-only employee, role `user`, status `disabled`; reject password/protected fields |
| `PATCH /api/admin/users/{id}` | Managed employee status, or explicit immutable OAuth conversion when identity headers are present |
| `POST /api/admin/projects` | Native creation with durable receipt |
| `POST/PATCH /api/admin/resources/{kind}[/{id}]` | Native teams, project-members, quota-policies and budgets with receipts |
| `POST /api/admin/projects/{id}/keys` | Native Key creation with replayable encrypted result |
| `PATCH /api/admin/projects/{id}` | Status-only change preserving other fields; ownership-safe restoration |
| `PATCH /api/admin/api-keys/{id}` | Status-only change; manual/expired/rotated Keys cannot be silently restored |
| `GET /api/admin/users` | Safe user metadata plus immutable `dop_identity` binding |
| `GET /api/admin/resources/quota-policies` | Raw policy catalog without per-policy usage scans |
| `GET /api/admin/resources/quota-policies?dop_user_id={id}` | One user's native current-period usage plus managed identity/Team proof |
| `GET /api/admin/resources/budgets?dop_team_id={id}` | One Team's `used_usd`, budgets, UTC period and timestamp |
| `GET /api/admin/api-keys?dop_owner_id={id}` | Owner-filtered Key metadata; no plaintext or hash |

Employee creation/conversion additionally requires `X-DOP-OAuth-Provider` and `X-DOP-OAuth-Subject` (base64url without padding of the exact UTF-8 IdP sub). The provider must be active. Bindings are immutable. Conversion removes local-password access and existing sessions; administrators cannot be converted. The original single-path parameter schema is otherwise retained.

A newly managed employee stays disabled until a positive user policy commits. Quota validation uses an operation-local view of a DOP-owned suspended employee, without enabling the stored account. Activation rechecks the committed policy. Native approvals and audit behavior remain in force; pending approval is never reported as applied. Key creation approvals still follow native one-time secret delivery; unresolved results require reconciliation of that original approval rather than a replacement Key.

## Batch changes and query proof

The existing eight routes remain. The additional platform-admin-only route is:

```http
POST /api/admin/dop/batch/users/status
Authorization: Bearer <ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: dop-user-status-operation

{"items":[{"client_ref":"employee-1","id":"tokenhub-user-1","body":{"status":"disabled"}},{"client_ref":"employee-2","id":"tokenhub-user-2","body":{"status":"disabled"}}]}
```

`status` supports `active` and `disabled`. Only managed ordinary employees are eligible. Re-enabling requires the initial positive quota or an unchanged DOP-owned disable marker. Results and errors use the existing per-item batch envelope. Limit is 20,000; omit `concurrency` to preserve serial processing. Explicit concurrency uses the existing bounded executor and resource dependencies.

User usage results additionally carry `dop_identity` with `provider_id`, `subject`, `team_id`, `team_name`, and `role`. This is fetched in the same set query as the requested users, maintaining bounded SQL query count. DOP must verify these against its tenant/unit and trusted IdP directory before returning usage or Key metadata; old browser-written associations are not authority.

Only the independent entry initializes the DOP tables and the lookup index described below. Preserve the encryption key and extension tables when upgrading. The original entry does not enable these endpoints or initialize these extensions.

## DOP lookup and transaction guarantees

Besides its three extension tables, startup creates `idx_dop_projects_quota_ref` on the whitespace-trimmed `projects.default_quota_ref`. This index bounds quota approval lookups; project status and Team budget operations read only their target rows. Existing columns and migration manifests are unchanged. Allow time for the first index build: it may block project writes until it completes. Back up the database, including the extension tables, before rollout.

On PostgreSQL, quota and budget PATCH operations lock their target before reading and merging fields, including default serial batches and DOP native single requests. User and Team usage reads use read-only repeatable-read snapshots. Audit insertion failures roll back quota, budget and Key changes together with their approvals and replay receipts. These guarantees do not change response formats or opt requests into concurrent execution.

## v0.10.0 compatibility

The DOP entrypoint retains upstream application dependency initialization. Within each batch item, native primary, critical, and analytics database reads share the receipt transaction so validation observes uncommitted changes and rollback remains atomic. The DOP image defaults to the upstream 0.10.0 bundle. Back up the database and preserve the server secret before upgrading: upstream migrations 8 and 9 advance the schema, and switching back to a 0.9.0 image alone is not a supported recovery procedure. Existing quota settings and DOP request formats are retained.
