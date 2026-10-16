# DOP 仅 OAuth 员工批量预建

通过 `cmd/tokenhub-dop` 启用，启动方式见 [DOP 批量接口](dop-batch-api.md)。仅创建新员工，不自动转换或关联已有账号。只改扩展文件，新增 `dop_oauth_identities` 表，不增加环境变量、不修改原有表。

## 完整请求

每批 1～12000 人，仍受默认 8 MiB JSON 请求体限制。`Authorization` 放平台管理员 Token，`Idempotency-Key` 与每项唯一的 `client_ref` 必填。每项不要传 `method`、`id`，员工对象不能传 `password`、`role`、`status`、`password_hash`；Key 的人员和项目由服务端确定。

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

| 参数 | 含义 |
| --- | --- |
| `username` | 可选，默认邮箱，去空白转小写，最多 128 字节 |
| `name` | 可选显示名，最多 256 字节 |
| `email` | 必填，去空白转小写，最多 254 字节；不用于自动关联已有账号 |
| `team_id` | 必填，二级单位 Team；原生事务锁校验团队存在且启用 |
| `project_id` | 必填，启用的 Project，主 Team 必须等于 `team_id` |
| `oauth_provider_id` | 必填，已配置且启用的 OAuth/OIDC 提供方 ID，最多 128 字节 |
| `oauth_subject` | 必填，与提供方 userinfo 返回的字符串 `sub` 完全一致，区分大小写，最多 512 字节 |
| `monthly_limit_usd` | 必填，有限正数，DOP 目标换算后的人员月度总上限，单位 USD |
| `key` | 必填对象；支持示例中的原生 Key 创建参数，人员归属由服务端设置 |

DOP 应从可信身份目录或 IdP 映射取得不可变的 `sub`，不能猜测“工号就是 sub”。本接口要求提供方返回稳定的字符串 `sub`；没有该字段的其他原生登录提供方需先增加明确适配。不同提供方的相同 sub 是不同身份。接口不提供身份重新绑定；原生删除员工后保留绑定墓碑，避免自动转给别人。

## 创建顺序、审批和重试

每个人在一个事务中依次执行：校验 → 创建普通员工账号（不计算 bcrypt）→ 绑定提供方/sub → 调用原生逻辑创建人员月额度 → 调用原生逻辑创建本人 Key → 加密回执并提交。保留团队校验、额度/Key 校验、审批、审计及原生字段含义，不发送重置密码邮件。Key 参数错误会回滚该员工的账号、绑定、额度和审计，不影响其他成功项。用户名/邮箱已存在、提供方/sub 重复会失败，不自动收编已有账号。

外层 HTTP200 表示全部生效，207 表示含待批或失败项。每项保留原有批量结果字段，`body` 包含 `user`、`authentication=oauth_only`、`stage`、`quota` 及已执行到的 `key`。成功时 `stage=ready`，保存 `user.id`、`quota.id`、`key.id`，并在 DOP 安全保存 `key.api_key`。响应丢失使用同一 key/ref 和相同内容重试；已提交结果和 Key 在 24 小时内可加密重放。重放是历史结果，不是最新状态；未知结果不要换新操作号重新创建。

- `stage=quota_pending`：额度尚未生效，不创建 Key，并在同一事务将新员工禁用，避免产生没有上限的可用身份。先处理返回的原生额度审批，确认实际人员正额度已生效，再 `PATCH /api/admin/users/{user_id}`，传 `{"status":"active","team_id":"<原Team>","team_ids":["<原Team>"]}`；最后用新的操作号调用 `POST /api/admin/dop/batch/api-keys/create`，人员 ID 使用已返回的 `user.id`。重试原预建回执不会自动续办后续阶段。
- `stage=key_pending`：人员额度已生效，Key 仍等原生审批；通过既有审批流程取得审批结果和一次性 Key，不重复预建账号。

## 登录保护边界

就绪员工保持正常启用。数据库密码字段写不可匹配的标记，不是空密码或统一可用密码，也不复用一份 bcrypt 哈希。预建不执行逐人密码计算。DOP 入口禁止受管员工密码登录、生成/使用密码重置令牌、管理员设置密码和提升角色；原生导入不能更新这些账号。管理员等其他本地账号仍保留原密码登录行为。

OAuth 继续使用原生提供方 token/userinfo 交互、state Cookie、一次性流程、提供方 PKCE 和浏览器 PKCE 换会话。受管员工按 `(provider_id, sub)` 选择账号，缺少/错误 sub 或其他提供方不能仅靠相同邮箱接管；提供方明确声明邮箱未验证时拒绝登录。绑定命中后不让 claims 覆盖 DOP 的姓名、邮箱、Team 和角色；禁用/删除员工不可登录。未预建的其他 OAuth 用户保留原版行为，首次自动建号仍会计算 bcrypt；本次优化针对新预建员工路径。

已有受管员工的数据库必须只使用 DOP 入口。原入口保留用于兼容上游，不执行这些登录保护，不应与 DOP 入口同时对外提供服务，也不能作为这些员工的直接回退方案。备份须包含扩展表，原版仅按模型导出 JSON 不足以恢复本扩展。合并上游后，除入口对齐测试外，还要运行 OAuth 回调的状态和 PKCE 回归测试。

## 性能验证

`TestDOPProvisionTwelveThousand` 在临时磁盘 SQLite 中通过单次 HTTP Handler 请求真实创建 12000 个账号、月额度和 Key，校验落库、结果顺序、Key 返回及整批最多两次完整人员读取；这 12000 名员工不是测试直接插表的夹具。消除密码计算瓶颈，不降低本地账号的密码强度。实际耗时见测试日志；网络、生产磁盘和 PostgreSQL 需另外实测。
