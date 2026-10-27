# DOP 原生单资源安全扩展

由独立 `cmd/tokenhub-dop` 入口启用。原版入口及源码保持不变。DOP 入口应用下述额度读取和用户删除优化。

DOP 入口的普通策略列表从同一个数据库快照分批读取用量；读取失败时返回脱敏的 500。DOP 认证和普通用户删除使用静态管理员令牌时，每次读取最早创建的用户，不使用永久缓存，数据库错误时拒绝请求。其他原生路由保留原认证方式。DOP 入口启动时创建管理员用户创建时间索引。

在 DOP 入口，`GET /api/admin/resources/quota-policies?id={id}` 按策略 ID 精确查询，并沿用原生认证、权限与可见范围过滤、用量计算和响应脱敏。已有调用方无需增加 DOP 请求头或更换 URL。ID 不存在或无权访问时返回空 `data` 数组；不传 `id` 或传空值时保留完整可见列表。带 `X-DOP-Operation: true` 时同样支持 ID 筛选，仍要求平台管理员，策略目录仍不计算用量。原版 `cmd/tokenhub` 入口保留原列表行为，包括传入 `id` 的情况。

必须以平台管理员 Bearer Token 认证。`GET /api/admin/dop/capabilities` 返回 `native_single_receipts`、`managed_single_users` 和版本 1。扩展写请求还必须传稳定的 `Idempotency-Key`（1～128 字符），同一标识换参数会拒绝。原生处理器在事务内执行并保存加密回执，不向单项批量 HTTP 接口转发。原回执窗口为 24 小时；DOP 应持久化加密结果，未知结果继续原操作。

## 支持的原生路径

- `POST /api/admin/users`：创建 role=user、status=disabled 的 OAuth 员工，禁止密码/受保护字段。
- `PATCH /api/admin/users/{id}`：受管员工状态；带身份头时显式迁移为不可变 OAuth 身份，撤销本地密码及旧会话，不能迁移管理员。
- `POST /api/admin/projects`，`POST/PATCH /api/admin/resources/{kind}[/{id}]`：原生创建/更新及回执，kind 为 teams、project-members、quota-policies、budgets。
- `POST /api/admin/projects/{id}/keys`：原生 Key 创建，原操作可重放同一密钥结果。
- `PATCH /api/admin/projects/{id}`、`PATCH /api/admin/api-keys/{id}`：仅变更状态，保护其他字段，只恢复 DOP 停用且未被人工修改的资源；过期/轮换 Key 不恢复。
- `GET /api/admin/users`：安全元数据加 `dop_identity`。
- `GET /api/admin/resources/quota-policies`：策略目录，不逐策略查询用量；加 `dop_user_id` 查询单人当前周期及身份归属证明。
- `GET /api/admin/resources/budgets?dop_team_id={id}`：单 Team 的 `used_usd`、预算、UTC 月份和时间戳。
- `GET /api/admin/api-keys?dop_owner_id={id}`：本人范围 Key 元数据，不含明文/哈希。

员工创建/迁移须传 `X-DOP-OAuth-Provider` 及 `X-DOP-OAuth-Subject`：后者为精确 IdP sub 的 UTF-8 base64url（无 padding），不能用邮箱/工号猜测，提供方须启用。除显式扩展头外保持原单资源参数结构。

新员工在正数策略生效前保持停用。额度校验仅在本次操作的视图中识别 DOP 持有的停用员工，不提前启用实际账号；随后启用再次核对有效策略。沿用原生审批和审计，待批不视为成功。Key 审批仍遵守原生一次性密钥交付；未取得结果须核对原审批，不能重复创建替代 Key。

## 新增批量员工状态与用量证明

保留已有八个批量路径，增加 `POST /api/admin/dop/batch/users/status`：

```json
{"items":[{"client_ref":"employee-1","id":"tokenhub-user-1","body":{"status":"disabled"}},{"client_ref":"employee-2","id":"tokenhub-user-2","body":{"status":"disabled"}}]}
```

请求头为 `Authorization: Bearer <ADMIN_TOKEN>`、`Content-Type: application/json`、稳定 `Idempotency-Key`。支持 active/disabled，仅受管普通员工；恢复需要初始正数额度或未改变的 DOP 停用凭据。沿用逐项状态/失败结果，每批最多 20,000。省略 concurrency 为原串行规则，显式设置才启用现有有界协程池及资源依赖保护。

用户用量结果增加 `dop_identity`，包含 provider_id、subject、team_id、team_name、role，与人员集合在同一次 SQL 联接查询读取，查询数不随人数线性增加。DOP 返回用量/Key 前必须核对当前租户单位及可信身份，不能相信旧前端账本中的关联。

仅独立入口初始化 DOP 表及下述查询索引；更新时保留密钥和扩展表。原入口不启用或初始化这些扩展。

## DOP 查询与事务保证

启动时除了创建三个扩展表，还会在去除首尾空白的 `projects.default_quota_ref` 上创建索引 `idx_dop_projects_quota_ref`，用于按目标查询额度审批关联项目；项目状态和团队预算操作只读取目标行。原有列和迁移清单保持不变。首次建索引可能在完成前阻塞项目写入，部署时应预留时间，并提前备份包含扩展表的数据库。

在 PostgreSQL 上，额度和预算 PATCH 在读取、合并字段前先锁定目标，包括默认串行批次和 DOP 原生单条请求。人员和团队用量查询使用只读可重复读快照。审计插入失败时，额度、预算、Key 变更及其审批、重放回执一并回滚。这些保证不改变响应格式，也不会自动启用并发执行。

## v0.10.0 兼容性

DOP 入口保留上游的应用依赖初始化。每个批量条目内，原生主连接、关键读取和分析读取都使用回执事务，使校验能看到尚未提交的变更，并保持原子回滚。DOP 镜像默认基于上游 0.10.0 安装包。升级前备份数据库并保留服务端密钥：上游迁移 8、9 会推进数据库版本，不能仅切回 0.9.0 镜像恢复。保留现有额度配置和 DOP 请求格式。
