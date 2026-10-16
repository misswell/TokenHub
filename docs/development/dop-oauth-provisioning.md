# DOP OAuth-only employee provisioning

Enable with `cmd/tokenhub-dop` as described in [DOP batch API](dop-batch-api.md). This opt-in route creates new employees; it does not convert or link existing accounts. It changes only extension files, introduces `dop_oauth_identities`, and adds no environment variables or changes to native tables.

## Complete request

The endpoint accepts 1–12000 items subject to the existing default 8 MiB JSON limit. Use a platform Admin Bearer Token, never an employee/model/analytics token. `Idempotency-Key` and each unique `client_ref` are required. Do not supply item `method`/`id`, `password`, `role`, `status`, `password_hash`, or Key owner/project overrides.

```http
POST http://localhost:8080/api/admin/dop/batch/users/provision HTTP/1.1
Authorization: Bearer <PLATFORM_ADMIN_TOKEN>
Content-Type: application/json
Idempotency-Key: employee-provision-20261016-001

{
  "items": [
    {
      "client_ref": "employee-00001",
      "body": {
        "username": "z00001",
        "name": "Employee 00001",
        "email": "z00001@example.com",
        "team_id": "team_company_001",
        "project_id": "proj_company_001",
        "oauth_provider_id": "idp_enterprise",
        "oauth_subject": "immutable-oidc-sub-00001",
        "monthly_limit_usd": 100,
        "key": {
          "name": "Employee 00001 key",
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
    }
  ]
}
```

| Parameter | Meaning |
| --- | --- |
| `username` | Optional; defaults to email; trimmed/lowercased; at most 128 bytes |
| `name` | Optional display name, at most 256 bytes |
| `email` | Required, trimmed/lowercased, at most 254 bytes; never used to link an existing account |
| `team_id` | Required active secondary-unit Team, validated by the native team lock |
| `project_id` | Required active Project whose primary Team equals `team_id` |
| `oauth_provider_id` | Required existing active configured OAuth/OIDC provider ID, at most 128 bytes |
| `oauth_subject` | Required exact case-sensitive OIDC userinfo `sub`, at most 512 bytes; never a display name, guessed username or email |
| `monthly_limit_usd` | Required finite positive User monthly target in USD, converted from the DOP target |
| `key` | Required object; native Key creation fields shown above; ownership is set by the server |

DOP must obtain the immutable subject from its trusted identity directory/IdP mapping before provisioning; do not guess it from an employee number. This route requires a provider returning a stable string `sub`. Native providers without that claim need an explicit adapter before using this route. The same subject under different providers is a different identity. Bindings cannot be retargeted through this API; deleting a native user leaves a binding tombstone and does not silently reassign it.

## Ordering, approvals and retries

Each employee runs in one transaction: validate → create active `user` identity without bcrypt → bind provider/sub → create User quota through the native handler → create owned Key through the native handler → encrypt receipt and commit. User creation, quota/Key validation, approval, native audit and native field semantics remain in force. No reset emails are sent. An invalid Key rolls back that employee's account, binding, quota and audits; other items continue. Existing usernames/emails and duplicate provider/sub pairs fail instead of silently adopting accounts.

Top-level HTTP200 means all applied; HTTP207 includes pending/failed items. Each result has the shared batch fields; its `body` contains `user`, `authentication=oauth_only`, `stage`, `quota`, and (when reached) `key`. On success `stage=ready`, `quota.id` is the policy ID and `key.api_key` is the secret. Store the returned User/policy/Key IDs and protected Key in DOP. Retry identical items with the same key/ref to recover committed results, including encrypted Key replay within 24 hours. Replay is historical, not a fresh status check. Never create a new operation key to retry an unknown result.

- `stage=quota_pending`: no quota is effective and no Key exists. The employee is disabled in the same transaction to avoid an unlimited active identity. Approve the returned native quota request first, verify the resulting positive User quota, then `PATCH /api/admin/users/{user_id}` with `{"status":"active","team_id":"<same Team>","team_ids":["<same Team>"]}`. Finally call `POST /api/admin/dop/batch/api-keys/create` with a new operation key and the returned user ID. Retrying the provision receipt does not continue later stages.
- `stage=key_pending`: User quota is effective, no Key is issued until native Key approval. Handle the native approval result and its one-time secret through the existing approval flow. Do not repeat provisioning.

## Authentication boundary

Managed employees keep `status=active` when ready; the local credential column contains an unmatchable marker, not an empty password or a shared password/hash. No per-user password hashing is performed. The DOP handler blocks their local password login, reset-token creation/use, administrator password changes and role elevation. Native import cannot update these accounts. Other local accounts, including administrators, retain their original password behavior.

OAuth uses the native provider token/userinfo exchange, state cookie, single-use flow, provider PKCE and browser PKCE exchange. Only the bound `(provider_id, sub)` selects a managed employee. Missing/wrong sub, another provider or unverified email cannot take over the employee by matching an email. An exact binding preserves the DOP profile, Team and role even if claims change. Disabled/deleted employees cannot log in. Other unmanaged OAuth accounts follow the native behavior, including bcrypt when created on first login; use provisioning for the optimized employee path.

Run only the DOP entrypoint for a database with managed employees. The original entrypoint is preserved for upstream compatibility but does not enforce the guards; do not serve it concurrently or roll back to it for these accounts. Use a full database backup including extension tables. Native model-only JSON export is insufficient. After upstream merges, test both entrypoint parity and the guarded OAuth callback against native flow changes.

## Performance validation

`TestDOPProvisionTwelveThousand` creates 12000 real identities, quotas and Keys through one HTTP handler request on a temporary disk SQLite database. It checks persistence, ordered results, secret presence and at most two full user reads for the entire request. There is no direct fixture insertion of the 12000 employees. This removes the measured bcrypt bottleneck without lowering password strength for local accounts. See test logs for actual elapsed time; network, production disks and PostgreSQL require separate measurement.
