# DOP バッチ API 拡張（追加ファイルのみ）

## 有効化

元のバックエンドと同じ保護された環境設定で、`backend/` から実行します。

```sh
go run ./cmd/tokenhub-dop
```

ビルド例：

```sh
go build -ldflags "-X main.buildVersion=0.9.3" -o ../.artifacts/tokenhub-dop ./cmd/tokenhub-dop
```

追加エントリーポイントは起動検証、移行/ロールバック保護、初期化、DB CLI、カタログと課金処理、正常終了を維持し、構成時のみ `Server.DOPBatchHandler()` を呼び出します。既存の入口、`Server.Handler()`、ルート、モデル、環境テンプレート、フロントエンドは変更しません。上流の起動処理が変わると入口整合テストが失敗するため、追加ファイルを同期してください。テキスト競合がなくてもアップグレード後の互換性確認は必要です。

有効化時に追加するテーブルは `dop_batch_receipts`、`dop_batch_status_marks`、`dop_oauth_identities` のみです。既存テーブルや移行マニフェストは変更しません。拡張テーブルを含むDB全体をバックアップしてください。元のモデルベースJSONエクスポートは拡張の再実行記録、復元マーカー、OAuth IDバインドを含みません。本番投入前に通常のバックアップとレビューを行ってください。OAuth専用従業員が存在する場合、ロールバック時もDOP入口と認証保護を維持してください。元の入口はローカルパスワード制限を適用しません。同じ管理DBに両入口を公開しないでください。この開発作業では共有サーバーへ配置しません。

## 動作と安全性

以下の7ルートはすべて `/api/admin/dop/batch/` 配下のPOSTです：`users/provision`、`users/usage`、`user-quotas`、`team-budgets`、`projects/status`、`api-keys/status`、`api-keys/create`。

ネイティブの `admin` / `system_admin` のみ利用でき、`Authorization: Bearer <PLATFORM_ADMIN_TOKEN>` または同等の管理者セッションが必要です。一般ユーザー、Team Leader、security_admin、分析トークン、モデルKeyは利用できません。DOPは実際の操作者の組織権限を別途検証し、認証情報をブラウザーに渡さないでください。

1リクエストは1～12000件で、既存のJSONバイト上限も適用します。最大12000人を一括送信できます。既存のJSON上限は既定8 MiBです。大きな項目は既存 TOKENHUB_MAX_JSON_REQUEST_BYTES とプロキシ/クライアント上限の調整が必要です。書き込みはサーバー内で各リソースを処理し、単一SQLや全員同時反映を保証しません。書き込み時は対象ユーザー/ポリシーだけを読み取り、全表の反復取得を避けます。用量読み取りは集合SQLで、Userの持続カウンターと過去のKey帰属集計の最大値を使用し、二重加算を避けます。

各項目の変更、承認、暗号化した再実行結果を同一トランザクションで保存します。失敗項目のみロールバックし、項目間の原子性はありません。全件成功はHTTP200、失敗または承認待ちは207です。`data[]` は入力順で、`client_ref`、`status`、`state`、`replayed`、`body` を含みます。202/pendingは未反映の承認待ちです。

書き込みには `Idempotency-Key` と各項目の一意な `client_ref`（各1～128文字）が必要です。管理者ID、ルート、キー、client_refの組で再実行を識別します。同じ項目内容を保持して未知/失敗項目だけ再送できます。保存済み結果は再作成せず返し、内容の変更は409です。未知の作成結果に対して新しいキーで盲目的に再試行しないでください。再実行結果は履歴であり、現在状態ではありません。

Key明文を含む結果はネイティブ `TOKENHUB_SECRET_KEY` で暗号化し、監査へ含めません。全応答は `Cache-Control: no-store` です。複数インスタンスと再起動で同じ暗号鍵を維持してください。再取得は24時間以内に同じ管理者から可能で、それ以降は410となり、再作成を防ぐ墓碑を保持します。暗号文/墓碑は自動削除しません。復号失敗時にも新しいKeyは作成しません。

DOPは目標、割当台帳、同期状態を先に保存し、その後USDに変換して呼び出します。`1 USD = R 三局豆` はDOPの換算です。自然月、TeamのUTC境界、USD通貨は変更しません。既存ポリシーと予算にはID付きPATCHを使用し、毎月の再チャージは不要です。

## 完全なリクエストとパラメーター

以下の共通プロトコル説明と完全な例は英語です。照会の `monthly_limit_usd` はDOP目標のUSD換算値で、余量計算専用、保存しません。省略すると余量も省略します。Keyの所有者は実際の従業員です。0は利用拒否を意味しないため停止APIを使います。拡張自身が停止し、その後変更のないリソースだけ復元でき、手動停止、期限切れ、ローテーション済みKeyは復元しません。Project設定は維持します。

OAuth専用従業員の完全な作成例と承認処理は [DOP OAuthプロビジョニング](dop-oauth-provisioning.md) を参照してください。bcryptを省略し、提供元とsubで結び付け、管理対象従業員のローカルパスワード操作を制限します。

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

## 上流アップグレード

上流をマージした後、入口整合テストとバッチテストを実行し、`cmd/tokenhub-dop` を再ビルドしてください。元の配布バイナリで置き換えると拡張ルートは有効になりません。上流の起動変更には追加入口だけを同期し、既存入口やルートファイルは編集しません。
