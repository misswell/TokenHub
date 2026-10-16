# DOP 批量接口扩展（仅新增文件）

## 启用方式

在 `backend/` 目录中，使用与原后端相同的受保护环境配置启动：

```sh
go run ./cmd/tokenhub-dop
```

构建独立入口：

```sh
go build -ldflags "-X main.buildVersion=0.9.3" -o ../.artifacts/tokenhub-dop ./cmd/tokenhub-dop
```

新增入口保留原版启动校验、迁移/回滚保护、初始化、数据库 CLI、目录与账单任务、优雅退出，仅在组装时调用 `Server.DOPBatchHandler()`。原入口、`Server.Handler()`、路由、原有模型、环境模板和前端文件均不修改，原接口继续可用。入口对齐测试会在上游启动流程变化时失败，届时只同步新增入口。仅新增文件可以避免文本冲突，但升级后仍须验证兼容性。

扩展启动时只新增数据库表 `dop_batch_receipts`（加密重放记录）、`dop_batch_status_marks`（停用来源标记）和 `dop_oauth_identities`（员工 OAuth 身份绑定），不修改原有表和迁移清单。备份应覆盖扩展表；原版按模型导出的 JSON 不包含本扩展的重放记录、恢复标记和 OAuth 身份绑定。生产发布前按部署流程备份及评审；已有仅 OAuth 员工时，回退仍须保留 DOP 入口和身份保护；原版入口不执行员工的本地密码限制。不得让两个入口同时访问同一个受管数据库。本次开发不自动部署共享服务器。

## 接口与处理规则

七个接口均使用 POST：

| `/api/admin/dop/batch/` 后的路径 | 用途 |
| --- | --- |
| `users/provision` | 批量预建仅 OAuth 员工，每人原子创建账号、月额度和 Key |
| `users/usage` | 指定人员集合的本日/本月已用及可选剩余额度 |
| `user-quotas` | 批量创建/调整人员月额度策略 |
| `team-budgets` | 批量创建/调整二级单位 Team 预算 |
| `projects/status` | 批量停用/恢复项目，保留项目原配置 |
| `api-keys/status` | 批量停用/恢复员工 Key |
| `api-keys/create` | 跨项目批量创建员工 Key |

仅原生 `admin` / `system_admin` 可调用，认证位置为 `Authorization: Bearer <PLATFORM_ADMIN_TOKEN>`，也接受同等平台管理员会话。普通员工、Team Leader、安全管理员、分析 Token 和模型 Key 均不能使用。DOP 自行校验实际操作人员的权限和组织范围；原生审计记录的是平台身份。后台凭证不交给浏览器。

每批 **1～12000 项**，并受原有 JSON 请求体大小限制。最多12000人可以一次提交；原有请求体默认上限仍为8 MiB，字段较大时需配置现有 TOKENHUB_MAX_JSON_REQUEST_BYTES，并同步代理/客户端限制。不承诺全部人员同时生效。批量写入在服务端逐项复用原接口逻辑，减少 HTTP 请求，不宣称所有变更合并成一条 SQL。写入时只读取当前项的目标人员/策略，避免逐项重复拉取整表。人员用量查询使用集合 SQL，查询次数不随人员数线性增长，按原生逻辑取 User 持久计数与 Key 历史归因汇总的最大值，避免重复累计。

每项的变更/审批和加密重放记录在同一事务中提交，失败只回滚该项，不是跨人员事务。HTTP 200 表示全部成功；207 表示存在失败或待审批项。返回 `data[]` 与输入顺序一致，含 `client_ref`、原生状态码 `status`、`state`（succeeded/pending/failed）、`replayed`、原生结果或错误 `body`。202/pending 只表示等待审批，不能标记为已生效。权限/外层参数错误可直接拒绝整批。

## 重试、金额和 Key 安全

写请求必须携带 `Idempotency-Key`（1～128 字符）；每项必须有唯一 `client_ref`（1～128 字符）。重放身份为“平台操作者 ID + 接口 + 请求键 + client_ref”。重试保持该项内容不变，可只重试失败/未知项，也可改变分批边界或排列顺序。已提交项返回原结果，不重复创建资源或审批；同一身份改内容返回409。新业务变更使用新键；不能为响应未知的创建操作盲目换键重试。重放是历史结果，不能据此推断资源当前状态。

结果使用原生 `TOKENHUB_SECRET_KEY` 加密，包括员工 Key 明文，不写入审计。响应带 `Cache-Control: no-store`。各实例和重启后须使用相同受保护密钥。24 小时内可由同一平台身份重放，之后返回410并保留防重复墓碑，不重新创建；密文/墓碑不自动清理，后续按运维保留策略管理。这是扩展提供的安全恢复能力，与原版只返回一次明文的语义不同。解密失败不会重新创建 Key。

DOP 先保存目标、分配账及同步状态，再调用接口并逐项记录结果。三局豆换算仍为 `1 USD = R 三局豆`；TokenHub 原生金额仍为 USD，不改重置日或 Team 的 UTC 月界。人员余额读取 DOP 的设置额度并减去原生已用，非 TokenHub 钱包。已有策略/预算必须传其 ID 使用 PATCH；普通月度续期不需发放任务。

## 完整调用示例与参数

下面保留统一的英文协议说明及完整请求，参数名与响应字段不翻译。要点：查询的 `monthly_limit_usd` 是 DOP 按比例换算后传入的目标，仅用于计算，不落库；省略则不返回余量。Key 的 `owner_user_id` 必须是实际员工。额度不能用0表示停用，应调用状态接口；扩展只恢复自己停用且未被改动的资源，不恢复手工停用、过期或已轮换 Key。项目状态更新保留 Team、owner、成本中心、模型设置及额度绑定。

## HTTP 请求示例

以下六个接口均使用 **POST** 和平台管理员认证。写操作还需 `Idempotency-Key`；`users/usage` 不需要。请替换地址、凭证、ID 和金额。金额为 **USD**，三局豆比例换算由 DOP 处理。以下是完整协议示例，未实际发送。

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

`users` 为1～12000个不重复的用户，ID 最长256字符。可选 `monthly_limit_usd` 为 DOP 设置额换算后的有限非负 USD，仅计算 `max(0, 设置额 − 已用)`，**不会落库，也不会读取或推断钱包余额**；不传则不返回设置额/余量字段。每项返回 `user_id`、`status`、`current_usage.daily/monthly`（请求数、输入/输出/总 Token 数、cost_usd），以及可选 `monthly_limit_usd`、`remaining_usd`。顶层 `periods.day/month`、`as_of`、`currency=USD` 给出原生周期及读取起始时点。用户不存在时该项404，存在但无计数时已用为0；数据库失败直接报错，不补假零。按员工所有 Key 的历史归因统计，非单项目或当前 Key。按输入顺序一次返回所请求集合，不需游标分页。

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

POST 新建策略；PATCH 必须传已有策略 ID。返回 ID 存入 DOP。body 不允许传 `id`，新增 ID 由服务器生成，避免覆盖任意原生资源。修改时保留原有 fields 和 description，不允许转移 scope_id，monthly_cost_usd 必须为正数。fields 可携带原生策略的其他字段，仍执行原生校验/审批。月内调额保留已用；金额目标持续适用，新月自动使用新计数，不需定时批量充值。审批项处于 pending 时尚未生效。

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

Team 必须存在，amount_usd 为正数，enforcement=hard。period_ref 为空时持续适用各自然月，2026-10 仅适用指定月份。PATCH 保留其他字段及说明。DOP 根组织“三局集团”没有 TokenHub 预算，不加入本批次。这些接口不修改 USD 计量、自然月或 Team 的 UTC 月界。

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

两个状态接口只接受 active / disabled，要求 id，无需 item.method。项目的 Team、owner、成本中心、模型访问及额度绑定均保留。已处于目标状态时返回 changed=false；原先手工停用的资源不会被本扩展接管。恢复要求匹配扩展停用标记、资源指纹和最后审计 ID；即便人工再次停用没有改变 Key 字段，也会阻止自动恢复。不恢复过期或已轮换失效 Key，被修改、手工停用或撤销的资源返回409。普通额度耗尽、自然跨月不需修改状态。零分配需通过状态接口阻断相关入口，原生非正金额不是拒绝消费规则。

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

project_id 和实际员工 owner_user_id 必填。其他参数沿用原生语义：inherit / restricted 模型模式、受限模式下已配置的模型 ID、可选 IP 白名单/分钟限制/有效期，以及 Key 自身限额。Key 层金额为0不取消 User 层策略。成功项 body 返回 id、api_key 及原生元数据，DOP 安全保存完整 Key，仅允许本人查看。每次调额不创建新 Key；更换 Key 但 owner 不变时，User 累计已用和额度不重置。

## 合并上游新版本

正常合并上游后，运行入口对齐测试和批量接口测试，再重新构建 `cmd/tokenhub-dop`。不要直接用原版发行包替换定制可执行文件，原入口不会启用这些扩展路由。若上游启动流程变化，只需同步本次新增入口，无须编辑原入口或路由文件。
