# DOP batch API extension (additive files)

## Enable without changing upstream files

Run from `backend/` using the same protected environment as the original backend:

```sh
go run ./cmd/tokenhub-dop
```

For a release build:

```sh
go build -ldflags "-X main.buildVersion=0.9.3" -o ../.artifacts/tokenhub-dop ./cmd/tokenhub-dop
```

The new `cmd/tokenhub-dop` preserves upstream startup validation, migration/rollback guard, bootstrap, database CLI, catalog/billing startup and graceful shutdown. Only its composition calls `Server.DOPBatchHandler()`. Original `cmd/tokenhub`, `Server.Handler()`, routes, models, environment templates and frontend are unchanged. Existing routes still work. The entrypoint parity test detects upstream startup drift; update the added entrypoint when that test fails after an upstream merge. Add-only files avoid textual conflicts but still require compatibility checks after upgrades.

Opt-in startup creates **only three extension tables**, `dop_batch_receipts`, `dop_batch_status_marks` and `dop_oauth_identities`, in the native database. Existing tables and migration manifests are not changed. Use a full database backup that includes these extension tables: the native model-based JSON export does not include extension receipts, restore markers or OAuth identity bindings. Back up the database and review normal deployment procedures before production rollout. For managed OAuth employees, retain the DOP entrypoint and identity guards during rollback; the original binary does not enforce their local-password policy. Never expose both entrypoints against the same managed database. This repository change does not deploy a shared server or change its configuration.

## Contract and processing

| Route after `/api/admin/dop/batch/` | Purpose |
| --- | --- |
| `users/provision` | Create OAuth-only employees with monthly quotas and Keys atomically per employee |
| `users/usage` | Explicit user collection, current-day/month native use and optional remaining amount |
| `user-quotas` | Create/update individual User monthly policies |
| `team-budgets` | Create/update Team budgets |
| `projects/status` | Disable/restore Projects while preserving their configuration |
| `api-keys/status` | Disable/restore employee Keys |
| `api-keys/create` | Create employee Keys across Projects |

Every route is server-to-server and requires native `admin` / `system_admin` identity via `Authorization: Bearer <PLATFORM_ADMIN_TOKEN>` (or equivalent platform admin session). Ordinary users, Team Leaders, security admins, analytical tokens and model Keys cannot use it. DOP must authenticate/authorize the human operator and record business identity itself; the native audit actor remains the platform identity. Secrets must never reach the browser or logs.

Each request contains 1–12000 entries and is also subject to the existing JSON byte limit. Up to 12,000 employees may be submitted in one request; the server still processes writes per item. The default JSON byte limit remains 8 MiB, including for this extension. Larger field contents may require the existing TOKENHUB_MAX_JSON_REQUEST_BYTES setting and matching proxy/client limits. The extension does not create an unbounded queue or promise all employees change simultaneously. **One HTTP request is now batch-capable; writes still execute per resource inside the server.** This reduces HTTP overhead but does not turn 10,000 writes into one SQL statement. User and policy lookups for writes load only the current item's targets instead of repeatedly scanning entire tables. Usage queries use set-based SQL with bounded query count, taking the max of durable User counters and attributed historical Key totals to match the native counter logic without double counting. Native billing limits and eventual/in-flight settlement still apply.

Writes reuse the original handlers and approval/audit logic on a transaction-scoped store. Each item's mutation, approval and encrypted receipt commit together. A failed item rolls back independently; earlier items remain committed. This is not a cross-item atomic transaction. HTTP 200 means all items succeeded; 207 means inspect failures and/or pending approvals. `data[]` preserves input order and includes `client_ref`, `status` (native HTTP status), `state` (`succeeded`, `pending`, `failed`), `replayed`, and `body` (native object or error). An item 202 means an approval is pending, **not applied success**. Validation/auth errors can fail the entire envelope before writes.

## Safe retries and secret storage

Every write item requires a unique nonempty `client_ref` (max 128 characters). Every write request requires a nonempty `Idempotency-Key` (max 128 characters). The identity is `(platform actor ID, route, Idempotency-Key, client_ref)`. Preserve the exact item content when retrying. Order or chunk boundaries may change; retrying only a failed/unknown subset is supported. A committed item replays its original result without creating a second resource or approval; changed content with the same identity returns 409. Failed/rolled-back items can be retried. A new business change requires a new key; **do not use a new key to blindly retry an uncertain create**. Replayed responses are historical results, not live resource state.

Results are encrypted using the native `TOKENHUB_SECRET_KEY`, including Key plaintext, and are not included in audits. All responses carry `Cache-Control: no-store`. Protect and retain the same encryption key across replicas and restarts. Replay is available for 24 hours. Afterward the identity remains a tombstone and returns 410; it does not recreate the resource. Ciphertext/tombstones are not automatically purged; define operational retention separately without deleting replay identities while retries remain possible. This opt-in recovery differs from native one-time Key response semantics: within 24 hours, the same authorized platform actor can recover its own committed result. Decryption errors never regenerate a Key.

DOP persists business targets, allocation ledger and sync status first, then calls these APIs and records each item result. Remaining amounts are computed from DOP's configured targets, not from a mutable TokenHub wallet. Save policy/budget/Key IDs. Positive amount changes retain current-month use. To update an existing resource use PATCH with its ID; do not POST duplicate policies for the same employee.

OAuth-only employee provisioning and its full request example are documented in [DOP OAuth provisioning](dop-oauth-provisioning.md). It skips bcrypt, binds the exact provider/subject and guards local password operations only for managed employees.

## HTTP examples

All seven routes use **POST** and platform administrator authentication. Write routes also require an `Idempotency-Key`; `users/usage` does not. Replace the URL, token, IDs and amounts. Amounts are **USD**; `1 USD = R 三局豆` conversion belongs to DOP. Examples below are protocol examples, not commands already executed.

```http
POST http://localhost:8080/api/admin/dop/batch/users/usage HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json

{
  "users": [
    {"user_id": "usr_001", "monthly_limit_usd": 100},
    {"user_id": "usr_002", "monthly_limit_usd": 50},
    {"user_id": "usr_003"}
  ]
}
```

`users` contains 1–12000 unique IDs, each at most 256 characters. `monthly_limit_usd` is optional, finite and nonnegative: supply the DOP target converted to USD. It is used only to compute `max(0, target - used)` and is **never persisted or inferred from a TokenHub wallet**. Omission means that both limit and remaining fields are omitted. Each result includes `user_id`, `status`, `current_usage.daily/monthly` (requests, prompt/completion/total tokens, cost_usd), and optional `monthly_limit_usd` / `remaining_usd`. Top-level `periods.day/month`, `as_of` and `currency=USD` describe the native quota period and read start time. Missing users have status 404; an existing user with no counters has zero use. Database errors fail the request instead of returning false zeroes. This reads all owned-Key attribution, not only one Project or current Key. It returns the requested users in input order; there is no cursor or hidden second page.

```http
POST http://localhost:8080/api/admin/dop/batch/user-quotas HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: quota-change-20261016-001

{
  "items": [
    {
      "client_ref": "employee-001",
      "method": "POST",
      "body": {
        "name": "Employee 001 monthly quota",
        "status": "active",
        "fields": {"scope": "user", "scope_id": "usr_001", "monthly_cost_usd": 100}
      }
    },
    {
      "client_ref": "employee-002",
      "method": "PATCH",
      "id": "qp_employee_002",
      "body": {"fields": {"monthly_cost_usd": 50}}
    }
  ]
}
```

POST creates a new policy; PATCH requires the existing policy ID. Save returned IDs in DOP. Do not submit a body `id`: POST uses server-generated IDs and cannot overwrite an arbitrary native resource. Updates preserve existing fields/description, cannot transfer `scope_id`, and require a positive numeric `monthly_cost_usd`. Other native policy fields may be supplied in `fields`; applicable native validation/approval still runs. Existing positive targets and spending are preserved across month boundaries; no monthly batch recharge job is needed. Native approvals can leave the item pending, without applying its quota.

```http
POST http://localhost:8080/api/admin/dop/batch/team-budgets HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: team-change-20261016-001

{
  "items": [
    {
      "client_ref": "company-001",
      "method": "POST",
      "body": {
        "name": "Company 001 monthly budget",
        "status": "active",
        "fields": {"scope": "team", "scope_id": "team_001", "period_ref": "", "amount_usd": 1000, "enforcement": "hard"}
      }
    },
    {
      "client_ref": "company-002",
      "method": "PATCH",
      "id": "budget_company_002",
      "body": {"fields": {"amount_usd": 2000}}
    }
  ]
}
```

The Team must exist. Use positive `amount_usd` and `enforcement=hard`. Empty `period_ref` repeats each natural month; `2026-10` targets that month only. PATCH retains other fields and description. The DOP root organization has no TokenHub budget and must never appear in this batch. These APIs do not change USD accounting, natural-month boundaries or the native Team UTC boundary.

```http
POST http://localhost:8080/api/admin/dop/batch/projects/status HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: reclaim-projects-20261016-001

{
  "items": [
    {"client_ref": "company-001", "id": "proj_001", "body": {"status": "disabled"}},
    {"client_ref": "company-002", "id": "proj_002", "body": {"status": "disabled"}}
  ]
}
```

```http
POST http://localhost:8080/api/admin/dop/batch/api-keys/status HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: restore-keys-20261016-001

{
  "items": [
    {"client_ref": "employee-001", "id": "key_001", "body": {"status": "active"}},
    {"client_ref": "employee-002", "id": "key_002", "body": {"status": "active"}}
  ]
}
```

Both status endpoints accept only `active` / `disabled`, require `id`, and do not need an item `method`. Projects retain their Team, owner, cost center, model access and quota binding. A no-op returns `changed=false`; a pre-existing manual disable is not adopted by DOP. Restore requires a matching extension disable marker, unchanged resource fingerprint and unchanged latest resource audit ID. A later manual disable is respected even if it leaves the public Key fields unchanged. Expired/rotated Keys cannot be restored; changed/manual/revoked resources return 409. Ordinary quota exhaustion or natural month rollover does not require status changes. Zero allocation must deny the relevant call entrances using status operations: a native nonpositive amount is not a deny rule.

```http
POST http://localhost:8080/api/admin/dop/batch/api-keys/create HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: employee-onboarding-20261016-001

{
  "items": [
    {
      "client_ref": "employee-001",
      "body": {
        "project_id": "proj_001",
        "owner_user_id": "usr_001",
        "name": "Employee 001 key",
        "group": "default",
        "model_access_mode": "inherit",
        "allowed_models": [],
        "ip_allowlist": [],
        "limits": {
          "daily_requests": 0,
          "monthly_requests": 0,
          "daily_tokens": 0,
          "monthly_tokens": 0,
          "daily_cost_usd": 0,
          "monthly_cost_usd": 0,
          "max_concurrency": 0
        },
        "rate_limit_rpm": null,
        "token_limit_tpm": null,
        "expires_at": null
      }
    }
  ]
}
```

`project_id` and actual employee `owner_user_id` are required. Other parameters retain their native create semantics: `inherit` / `restricted` model mode, configured model IDs for restrictions, optional allowlist/minute limits/expiration, and Key-local limits. A zero Key-local cap does not remove the User policy. Successful item `body` includes `id`, `api_key` and the native metadata. Keep the full Key in DOP's protected storage and restrict display to its owner. Do not create another Key for each quota adjustment. Changing Key with the same owner preserves the User-level accumulated spending and cap.

## Upstream upgrades

Merge upstream normally, run the entrypoint parity test and batch tests, then rebuild `cmd/tokenhub-dop`. Do not replace the custom executable with the upstream release binary: its unchanged entrypoint does not enable these routes. An upstream startup change may require synchronizing only the added entrypoint; no original file needs to be edited.
