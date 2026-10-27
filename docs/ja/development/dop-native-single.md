# DOP ネイティブ単一リソース安全拡張

独立した `cmd/tokenhub-dop` エントリで有効になります。元のエントリとソースは変更せず、DOP エントリにクォータ取得とユーザー削除の最適化を適用します。

DOP エントリの通常のポリシー一覧は同一スナップショットから使用量をバッチ取得し、失敗時は機密情報を除いた 500 を返します。DOP 認証と通常のユーザー削除では、静的管理者トークンのユーザーを永続キャッシュせず毎回取得し、データベースエラー時は拒否します。他のネイティブ経路は従来の認証を維持します。DOP エントリは起動時に管理者の作成日時インデックスを作成します。

DOP エントリでは `GET /api/admin/resources/quota-policies?id={id}` を ID の完全一致で検索し、元の認証、権限、可視範囲、使用量計算、レスポンスの秘匿化を維持します。既存の呼び出し元は DOP ヘッダーの追加や URL の変更が不要です。存在しない ID やアクセスできない ID には空の `data` 配列を返し、`id` の省略や空値では可視範囲の全一覧を維持します。`X-DOP-Operation: true` でも同じ ID フィルターを適用し、プラットフォーム管理者への制限と使用量計算の省略を維持します。元の `cmd/tokenhub` エントリでは、`id` を指定した場合も元の一覧動作を維持します。

プラットフォーム管理者の Bearer Token が必要です。`GET /api/admin/dop/capabilities` は `native_single_receipts`、`managed_single_users`、version 1 を返します。書き込みには固定の `Idempotency-Key`（1～128文字）が必要で、同じ識別子で異なる内容を送ると拒否します。元のハンドラーをトランザクション内で実行し暗号化した結果を保存します。単一要素のバッチ HTTP 呼び出しには変換しません。元の再取得期間は24時間です。DOP は暗号化した結果を永続化し、不明な結果には元の操作を再利用してください。

## ネイティブパス

- `POST /api/admin/users`：role=user、status=disabled の OAuth 専用従業員。パスワードと保護フィールドは不可。
- `PATCH /api/admin/users/{id}`：管理対象従業員の状態。ID ヘッダー付きの場合は変更不能な OAuth ID へ明示変換し、ローカルパスワードと既存セッションを無効化。管理者は変換不可。
- `POST /api/admin/projects` と `POST/PATCH /api/admin/resources/{kind}[/{id}]`：元の作成・更新処理と永続結果。kind は teams、project-members、quota-policies、budgets。
- `POST /api/admin/projects/{id}/keys`：元の Key 作成と同じ秘密情報の再取得。
- `PATCH /api/admin/projects/{id}` と `PATCH /api/admin/api-keys/{id}`：状態のみ変更し他のフィールドを保持。DOP が停止した未変更のリソースのみ復元。期限切れ・ローテーション済み Key は復元不可。
- `GET /api/admin/users`：安全なメタデータと `dop_identity`。
- `GET /api/admin/resources/quota-policies`：ポリシーごとの使用量走査を省略。`dop_user_id` を付けると1人の現在期間の使用量と所属証明。
- `GET /api/admin/resources/budgets?dop_team_id={id}`：Team の `used_usd`、予算、UTC 月、時刻。
- `GET /api/admin/api-keys?dop_owner_id={id}`：所有者で絞った Key メタデータ。平文やハッシュは返しません。

従業員の作成・変換には `X-DOP-OAuth-Provider` と `X-DOP-OAuth-Subject` が必要です。後者は正確な IdP sub の UTF-8 base64url（padding なし）です。メールや社員番号から推測せず、有効なプロバイダーを使用します。拡張ヘッダー以外は元の単一リソース形式を保ちます。

正のポリシーが確定するまで新しい従業員は無効です。DOP 所有の停止従業員を今回の検証用ビューだけで有効として扱い、実際のアカウントは先に開放しません。有効化時に確定済みポリシーを再検証します。元の承認と監査を維持し、承認待ちを成功として返しません。Key 承認の秘密情報は元の一度きりの受け渡し規則に従います。結果不明の場合、代替 Key を重複作成せず元の承認を照合してください。

## 従業員状態バッチと使用量の証明

既存8パスを維持し、`POST /api/admin/dop/batch/users/status` を追加します。

```json
{"items":[{"client_ref":"employee-1","id":"tokenhub-user-1","body":{"status":"disabled"}},{"client_ref":"employee-2","id":"tokenhub-user-2","body":{"status":"disabled"}}]}
```

`Authorization: Bearer <ADMIN_TOKEN>`、`Content-Type: application/json`、固定 `Idempotency-Key` が必要です。active/disabled をサポートし、管理対象の一般従業員に限定します。復元には初回の正の予算、または変更されていない DOP の停止証明が必要です。項目別の結果とエラーを返し、最大20,000件です。concurrency を省略すると元の逐次処理を維持し、明示指定時のみ既存の上限付き実行器とリソース依存保護を使います。

使用量結果の `dop_identity` は provider_id、subject、team_id、team_name、role を含みます。対象ユーザーとの同じ集合 SQL 結合で取得し、人数に比例してクエリ数は増えません。DOP は使用量や Key を返す前に、自テナントの組織と信頼済み ID に照合してください。旧ブラウザー台帳の関連付けだけを信用しないでください。

独立エントリだけが DOP テーブルと後述する検索インデックスを初期化します。更新時には暗号鍵と拡張テーブルを保持してください。元のエントリではこれらの拡張を有効化または初期化しません。

## DOP の検索とトランザクション保証

起動時に3つの拡張テーブルに加え、前後の空白を除いた `projects.default_quota_ref` に `idx_dop_projects_quota_ref` インデックスを作成します。クォータ承認に関連するプロジェクトを限定検索し、プロジェクト状態と Team 予算の操作は対象行だけを取得します。既存列と移行マニフェストは変更しません。初回のインデックス作成は完了までプロジェクト書き込みをブロックする可能性があるため、配置時に時間を確保し、拡張テーブルを含むデータベースを事前にバックアップしてください。

PostgreSQL では、クォータと予算の PATCH はフィールドの読み取りと統合前に対象をロックします。既定の逐次バッチと DOP ネイティブ単一要求も対象です。User と Team の用量読み取りは読み取り専用の repeatable-read スナップショットを使用します。監査挿入が失敗すると、クォータ、予算、Key の変更と承認、再実行結果をまとめてロールバックします。応答形式は変わらず、並行実行が自動で有効になることもありません。

## v0.10.0 の互換性

DOP エントリーポイントは上流のアプリケーション依存関係の初期化を維持します。各バッチ項目では、ネイティブの主接続、重要な読み取り、分析読み取りがレシートのトランザクションを共有し、未コミットの変更を検証でき、ロールバックの原子性を維持します。DOP イメージの既定は上流の 0.10.0 バンドルです。アップグレード前にデータベースをバックアップし、サーバーの秘密鍵を保持してください。上流のマイグレーション 8 と 9 はスキーマを更新するため、0.9.0 イメージへ戻すだけでは復旧できません。既存のクォータ設定と DOP リクエスト形式は維持します。
