# DOP OAuth専用従業員のバッチ作成

[DOPバッチAPI](dop-batch-api.md) の `cmd/tokenhub-dop` で有効化します。新規従業員のみ対象で、既存アカウントの変換・自動リンクは行いません。変更は拡張ファイルのみで、`dop_oauth_identities` テーブルを追加します。既存テーブルや環境変数は変更しません。

## 完全なリクエスト

1回1～12000件、既定のJSON上限8 MiBも適用されます。プラットフォーム管理者Bearer Token、`Idempotency-Key`、一意の `client_ref` が必要です。項目の `method`/`id`、パスワード・role・status・password_hash、Keyの所有者/Project上書きは指定できません。

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

| パラメーター | 意味 |
| --- | --- |
| `username` | 任意、既定email、空白除去・小文字化、最大128バイト |
| `name` | 任意表示名、最大256バイト |
| `email` | 必須、空白除去・小文字化、最大254バイト。既存アカウントのリンクには使わない |
| `team_id` | 必須、有効な二級単位Team。ネイティブのチームロックで検証 |
| `project_id` | 必須、有効なProject。主Teamが `team_id` と一致 |
| `oauth_provider_id` | 必須、設定済みの有効OAuth/OIDC提供元ID、最大128バイト |
| `oauth_subject` | 必須、userinfoの文字列 `sub` と完全一致、大文字小文字を区別、最大512バイト |
| `monthly_limit_usd` | 必須、有限の正数。DOP目標をUSDへ換算したUser月次上限 |
| `key` | 必須、上記ネイティブKey作成パラメーター。所有者はサーバーが設定 |

DOPは信頼済みIdP/ディレクトリーから不変のsubを取得してください。社員番号やメールから推測してはいけません。安定した文字列subを返さない提供元は明示的アダプターが必要です。同じsubでも提供元が異なれば別IDです。再バインドは提供せず、ユーザー削除後も墓碑を残して自動再割当を防ぎます。

## 順序・承認・再試行

各従業員のトランザクションで検証→bcryptなしの一般User作成→提供元/sub登録→ネイティブUser月次クォータ作成→所有Key作成→暗号化結果保存を実行します。チーム、クォータ/Keyの検証、承認、監査、ネイティブフィールド仕様を維持し、リセットメールは送りません。Keyエラー時はその従業員のUser、バインド、クォータ、監査をロールバックし、他項目は続行します。既存のユーザー名/メール、重複する提供元/subは自動リンクせず失敗します。

全適用はHTTP200、承認待ち/失敗を含む場合207です。共通項目結果の `body` は `user`、`authentication=oauth_only`、`stage`、`quota`、到達時の `key` を含みます。成功時は `stage=ready`、`user.id`、`quota.id`、`key.id` と保護された `key.api_key` をDOPへ保存します。応答喪失時は同じkey/refと内容で再試行し、24時間以内に暗号化済みKeyを再取得できます。再実行は履歴で、現在状態ではありません。不明結果に新しい操作キーを使わないでください。

- `quota_pending`：クォータ未適用、Keyなし。同じトランザクションでUserを無効化し、無制限の利用可能IDを防ぎます。返却されたネイティブ承認を適用し正のUserクォータを確認した後、`PATCH /api/admin/users/{user_id}` に `{"status":"active","team_id":"<same Team>","team_ids":["<same Team>"]}` を送り、別操作キーの `POST /api/admin/dop/batch/api-keys/create` で返却UserのKeyを作成します。元の作成結果を再取得しても後続段階は進みません。
- `key_pending`：Userクォータ適用済み、Keyはネイティブ承認待ち。既存の承認フローで結果と一度限りのKeyを取得し、作成を繰り返さないでください。

## 認証境界

準備完了Userはactiveのまま、ローカル資格情報は照合不能なマーカーです。空パスワードでも共有パスワード/ハッシュでもなく、個別bcrypt計算はありません。DOP入口は管理対象従業員のパスワードログイン、リセットトークン作成・使用、管理者によるパスワード設定、権限昇格を拒否します。ネイティブimportも更新できません。他のローカルアカウントと管理者のパスワード動作は維持します。

OAuthはネイティブtoken/userinfo通信、state Cookie、単回フロー、提供元PKCE、ブラウザーPKCE交換を維持します。管理対象は提供元IDとsubで選択し、sub欠落・不一致、別提供元、未検証メールでは同じメールの従業員を乗っ取れません。正しいバインドでもclaimsがDOPのプロフィール、Team、roleを上書きしません。無効/削除Userはログイン不可です。未管理OAuthアカウントはネイティブ動作を維持し、初回自動作成ではbcryptが残ります。最適化はこの事前作成経路です。

管理対象従業員のDBはDOP入口のみで提供してください。元の入口は保護を実行しないため同時公開や直接ロールバックに使えません。拡張テーブルを含むDB全体をバックアップし、ネイティブモデルのみのJSONエクスポートには依存しないでください。上流更新後は入口差分とOAuth callbackのstate/PKCE互換テストを実行します。

## 性能検証

`TestDOPProvisionTwelveThousand` は一時ディスクSQLiteと単一HTTP Handlerリクエストで12000個の実User・クォータ・Keyを作成し、永続化、順序、Key、全ユーザー読み取り最大2回を検証します。12000Userを直接fixture挿入しません。ローカルアカウントの暗号強度を下げずにbcryptボトルネックを除去します。実測時間はログを参照し、ネットワーク、本番ディスク、PostgreSQLは別途測定してください。
