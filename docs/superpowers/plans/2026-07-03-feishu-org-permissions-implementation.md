# Feishu Org Permissions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` when executing this plan. If a single agent executes inline, use `superpowers:executing-plans` and complete the checklist task by task. Do not skip the test-first steps.

**Goal:** 为公司内部使用场景接入飞书登录和飞书组织架构权限。员工通过飞书内部应用登录；系统从飞书同步部门、员工状态、部门负责人关系；超管给部门配置可用模型分组池；部门负责人只能给自己负责范围内的员工分配该部门已授权分组；超管仍可给单个员工做个人额外授权。

**Architecture:** 在现有 Sub2API 后端中新增 `feishu` 作为一等认证来源，复用 `auth_identities` 做已有用户精确绑定，复用现有 `auth_source_default_*` settings key 模式做飞书自动创建用户默认权益。组织架构作为飞书只读镜像保存，本系统不维护部门树、员工归属、上下级。权限生效层统一落到现有 `user_allowed_groups`，新增部门池、部门负责人授权、个人额外授权和审计表来解释权限来源。

**Tech Stack:** Go + Ent + Gin 后端；Vue 3 + TypeScript + Vite 前端；现有 Makefile 的 `make test-backend`、`make test-frontend-critical`、`make build` 作为主要验证入口。

---

## 当前约束

- 当前分支：`feat/feishu-org-permissions`。
- 设计文档：`docs/superpowers/specs/2026-07-02-feishu-org-permissions-design.md`。
- 不把飞书返回的邮箱或姓名作为首次登录自动绑定依据。第一版已有用户只通过预写入的 `auth_identities(provider_type=feishu, provider_key=tenant_key, provider_subject=open_id)` 精确匹配。
- 不在 Sub2API 维护组织架构。部门、员工归属、负责人和离职/禁用状态只从飞书同步。
- 飞书离职、禁用、移出可用范围后，默认自动禁用本地用户；触发批量保护或最后 active 超管保护时进入待确认。
- 被本地禁用的用户必须同时阻断网页登录、JWT、API Key 调用和 OAuth 会话刷新。
- `tools/feishu-login-demo/` 是临时验证工具，里面可能有本地测试凭证。实现主功能时不要把里面的密钥写进代码、测试、日志或提交信息。

---

## 数据和权限模型

### 现有表扩展

- `users.signup_source` 增加枚举值 `feishu`。
- `auth_identities.provider_type` 增加枚举值 `feishu`。
- 后台用户身份绑定接口允许 `provider_type=feishu`，字段含义：
  - `provider_key`：飞书 `tenant_key`。
  - `provider_subject`：飞书 `open_id`。
  - `provider_union_id`：如果现有 schema 没有该字段，第一版存到 `raw_data` 或等价 JSON 字段。

### 新增表

- `feishu_departments`：飞书部门只读镜像。
- `feishu_user_profiles`：飞书用户只读镜像和本地用户映射。
- `department_group_policies`：超管给部门授权的分组池。
- `department_managers`：飞书负责人关系投影。
- `user_group_grants`：用户分组授权来源明细。
- `org_permission_audit_logs`：组织权限审计日志。
- `feishu_org_sync_runs`：飞书组织同步运行记录、健康检查和影响预览。

### 权限生效规则

- 超管不受部门范围限制。
- 部门负责人 `B` 给员工 `A` 分配或取消分组 `G` 时必须同时满足：
  - `B` 是 active 用户。
  - `B` 来自飞书同步结果，负责 `A` 的主部门或其上级部门范围。
  - `A` 是 active 用户，并归属于 `B` 可管理的部门范围。
  - `G` 属于 `A` 所在部门或其继承部门池。
  - 本次修改不会移除来源为 `super_admin_override` 的个人额外授权。
- `user_allowed_groups` 是最终运行时读取表；`user_group_grants` 是解释来源和重算依据。

---

## 任务 1：提交前基线检查和计划落盘

- [ ] 确认工作树只包含设计文档、计划文档、飞书 demo 目录和本次实现改动：

  ```bash
  git status --short --branch
  ```

- [ ] 不提交 `tools/feishu-login-demo/.env`、本地日志、浏览器截图和任何密钥。
- [ ] 对计划文档运行占位符检查：

  ```bash
  bad_terms='T''ODO|T''BD|待''补|待''定|稍''后|以''后再|place''holder|fill'' in'
  rg -n --pcre2 "$bad_terms" docs/superpowers/plans/2026-07-03-feishu-org-permissions-implementation.md
  ```

- [ ] 保存设计文档和计划文档后提交一次文档基线：

  ```bash
  git add docs/superpowers/specs/2026-07-02-feishu-org-permissions-design.md docs/superpowers/plans/2026-07-03-feishu-org-permissions-implementation.md
  git commit -m "docs: add feishu org permission implementation plan"
  ```

---

## 任务 2：后端增加 `feishu` provider 和来源默认权益

### 先写测试

- [ ] 在 `backend/internal/service/setting_service_auth_source_defaults_test.go` 或现有同类测试文件中新增测试：
  - `TestAuthSourceDefaultSettingsIncludesFeishu`
  - `TestUpdateAuthSourceDefaultSettingsPersistsFeishu`
  - `TestResolveAuthSourceDefaultsFallsBackWhenFeishuUnset`
- [ ] 在 `backend/internal/service/admin_service_test.go` 增加测试：
  - `TestBindUserAuthIdentityAcceptsFeishu`
  - `TestBindUserAuthIdentityRejectsUnknownProvider`
- [ ] 测试断言：
  - `AuthSourceDefaultSettings` 返回结构包含 `Feishu`。
  - `feishu` 默认余额、并发、订阅、注册发放、首次绑定发放能保存和读取。
  - 管理员绑定身份时 `provider_type=feishu` 允许通过。
  - 未知 provider 仍拒绝。

运行预期失败：

```bash
cd backend && go test ./internal/service -run 'Test(AuthSourceDefaultSettingsIncludesFeishu|UpdateAuthSourceDefaultSettingsPersistsFeishu|ResolveAuthSourceDefaultsFallsBackWhenFeishuUnset|BindUserAuthIdentityAcceptsFeishu|BindUserAuthIdentityRejectsUnknownProvider)'
```

### 实现

- [ ] 修改 `backend/ent/schema/user.go`：`signup_source` enum 增加 `feishu`。
- [ ] 修改 `backend/ent/schema/auth_identity.go`：`provider_type` enum 增加 `feishu`。
- [ ] 修改 `backend/internal/service/domain_constants.go`：
  - 增加 `AuthSourceFeishu = "feishu"`。
  - 增加 `SettingKeyAuthSourceDefaultFeishuBalance`。
  - 增加 `SettingKeyAuthSourceDefaultFeishuConcurrency`。
  - 增加 `SettingKeyAuthSourceDefaultFeishuSubscriptions`。
  - 增加 `SettingKeyAuthSourceDefaultFeishuGrantOnSignup`。
  - 增加 `SettingKeyAuthSourceDefaultFeishuGrantOnFirstBind`。
  - 增加 `SettingKeyAuthSourceDefaultFeishuPlatformQuotas`，使用现有 `SettingKeyAuthSourcePlatformQuotas("feishu")` 形式。
- [ ] 修改 `backend/internal/service/setting_service.go`：
  - `AuthSourceDefaultSettings` 增加 `Feishu ProviderDefaultGrantSettings`。
  - 默认 settings map 增加飞书默认值：余额 `0`、并发 `5`、订阅 `[]`、注册发放 `false`、首次绑定发放 `false`。
  - `GetAuthSourceDefaultSettings` 解析 `feishuAuthSourceDefaultKeys`。
  - `UpdateAuthSourceDefaultSettings` 写入 `settings.Feishu`。
- [ ] 修改 `backend/internal/service/admin_service.go`：
  - `normalizeAdminAuthIdentityProviderType` 支持 `feishu`。
  - 绑定时继续依赖现有唯一约束，保证一个飞书身份只绑定一个本地用户。

### 验证和提交

- [ ] 运行本任务测试：

  ```bash
  cd backend && go test ./internal/service -run 'Test(AuthSourceDefaultSettingsIncludesFeishu|UpdateAuthSourceDefaultSettingsPersistsFeishu|ResolveAuthSourceDefaultsFallsBackWhenFeishuUnset|BindUserAuthIdentityAcceptsFeishu|BindUserAuthIdentityRejectsUnknownProvider)'
  ```

- [ ] 运行 Ent 生成或仓库已有生成命令。如果项目没有脚本，使用后端现有命令：

  ```bash
  cd backend && go generate ./ent
  ```

- [ ] 若 Ent 生成命令不适用，使用项目现有 README 或 Makefile 中的生成方式，并在提交说明里写清实际命令。
- [ ] 提交：

  ```bash
  git add backend/ent backend/internal/service
  git commit -m "feat: add feishu auth source defaults"
  ```

---

## 任务 3：后端增加飞书设置、登录入口保护和预检接口

### 先写测试

- [ ] 在 `backend/internal/service/setting_service_feishu_test.go` 新增：
  - `TestGetFeishuConnectSettingsDefaults`
  - `TestUpdateSecuritySettingsRejectsNoLoginEntry`
  - `TestUpdateSecuritySettingsRejectsFeishuOnlyWithoutHealthyConfig`
  - `TestUpdateSecuritySettingsAllowsAdminEmailFallback`
- [ ] 在 `backend/internal/handler/admin/setting_handler_feishu_test.go` 新增：
  - `TestAdminSettingsExposeFeishuConnectFields`
  - `TestFeishuPreflightReportsMissingCredential`
  - `TestFeishuPreflightReportsDefaultGrantWarning`
- [ ] 测试断言：
  - `email_password_login_enabled=false` 且 `feishu_connect_enabled=false` 时拒绝保存。
  - 只剩飞书登录但 App ID、App Secret、Redirect URL 缺失时拒绝保存。
  - 保留超管邮箱备用登录时，后台仍返回风险警告。
  - 预检接口返回能力拆分状态：登录、邮箱、组织同步、离职识别、负责人关系。

运行预期失败：

```bash
cd backend && go test ./internal/service ./internal/handler/admin -run 'Test(GetFeishuConnectSettingsDefaults|UpdateSecuritySettingsRejectsNoLoginEntry|UpdateSecuritySettingsRejectsFeishuOnlyWithoutHealthyConfig|UpdateSecuritySettingsAllowsAdminEmailFallback|AdminSettingsExposeFeishuConnectFields|FeishuPreflightReportsMissingCredential|FeishuPreflightReportsDefaultGrantWarning)'
```

### 实现

- [ ] 修改 `backend/internal/service/domain_constants.go` 增加设置键：
  - `feishu_connect_enabled`
  - `feishu_connect_app_id`
  - `feishu_connect_app_secret`
  - `feishu_connect_redirect_url`
  - `feishu_connect_tenant_restriction_policy`
  - `feishu_connect_allowed_tenant_key`
  - `feishu_connect_bypass_registration`
  - `feishu_connect_sync_email`
  - `feishu_connect_sync_display_name`
  - `feishu_connect_sync_department`
  - `feishu_org_sync_enabled`
  - `feishu_departed_user_action`
  - `feishu_sync_disable_threshold_count`
  - `feishu_sync_disable_threshold_percent`
  - `email_password_login_enabled`
  - `admin_email_login_fallback_enabled`
- [ ] 如果已有邮箱登录开关命名不同，保留既有 key，新增兼容映射，不迁移历史设置。
- [ ] 修改 `backend/internal/service/setting_service.go`：
  - 增加 `FeishuConnectSettings` 结构。
  - 增加读取飞书登录最终生效配置的方法 `GetFeishuConnectOAuthConfig(ctx)`。
  - 增加 `ValidateLoginEntryAvailability(ctx, settings)`，在保存安全与认证设置时调用。
  - 增加 `CheckFeishuPreflight(ctx)`，第一版至少做配置完整性、默认权益配置、最近同步状态检查。
- [ ] 修改 admin settings handler：
  - GET settings 返回飞书配置，App Secret 只返回 configured 布尔值，不回显密文。
  - PATCH/PUT settings 支持保存飞书配置和登录入口开关。
  - 新增 `POST /api/v1/admin/settings/feishu/preflight`。
- [ ] 错误文案使用清晰业务码：
  - `LOGIN_ENTRY_UNAVAILABLE`
  - `FEISHU_CONFIG_INCOMPLETE`
  - `ADMIN_LOGIN_FALLBACK_REQUIRED`

### 验证和提交

- [ ] 运行本任务测试：

  ```bash
  cd backend && go test ./internal/service ./internal/handler/admin -run 'Test(GetFeishuConnectSettingsDefaults|UpdateSecuritySettingsRejectsNoLoginEntry|UpdateSecuritySettingsRejectsFeishuOnlyWithoutHealthyConfig|UpdateSecuritySettingsAllowsAdminEmailFallback|AdminSettingsExposeFeishuConnectFields|FeishuPreflightReportsMissingCredential|FeishuPreflightReportsDefaultGrantWarning)'
  ```

- [ ] 运行后端关键设置测试：

  ```bash
  cd backend && go test ./internal/service ./internal/handler/admin
  ```

- [ ] 提交：

  ```bash
  git add backend/internal/service backend/internal/handler/admin
  git commit -m "feat: add feishu login settings guard"
  ```

---

## 任务 4：飞书 OAuth 登录和精确身份绑定

### 先写测试

- [ ] 参考 `backend/internal/handler/auth_dingtalk_oauth_test.go` 新增 `backend/internal/handler/auth_feishu_oauth_test.go`：
  - `TestFeishuOAuthStartDisabled`
  - `TestFeishuOAuthCallbackBindsExistingIdentity`
  - `TestFeishuOAuthCallbackDoesNotBindByEmail`
  - `TestFeishuOAuthCallbackCreatesUserWithFeishuDefaults`
  - `TestFeishuOAuthCallbackRejectsUnboundWhenAutoCreateDisabled`
  - `TestFeishuOAuthCallbackRejectsDisabledLocalUser`
  - `TestFeishuOAuthCallbackRejectsWrongTenant`
- [ ] 参考 `backend/internal/handler/auth_dingtalk_client_test.go` 新增 `backend/internal/handler/auth_feishu_client_test.go`：
  - `TestFeishuExchangeCodeParsesUserInfo`
  - `TestFeishuUserInfoAllowsEmptyEmail`
  - `TestFeishuUserInfoCapturesTenantKeyOpenIDUnionID`
- [ ] 测试断言：
  - OAuth disabled 时 start/callback 全部拒绝。
  - 已预写 `auth_identities(provider_type=feishu, provider_key=tenant_key, provider_subject=open_id)` 时登录到同一个本地用户，保留原余额、API Key、分组和订阅。
  - 飞书返回 email 为空或与本地 email 相同，都不触发邮箱自动绑定。
  - 自动创建用户时 `signup_source=feishu`，并应用 `settings.Feishu` 默认权益。

运行预期失败：

```bash
cd backend && go test ./internal/handler -run 'TestFeishu'
```

### 实现

- [ ] 新增 `backend/internal/handler/auth_feishu_client.go`：
  - 构造授权 URL。
  - 用 code 换 access token。
  - 调用飞书用户身份接口读取 `open_id`、`union_id`、`tenant_key`、`name`、`email`。
  - 支持 email 为空。
  - 所有日志只记录 request id、租户、open_id 前后缀，不记录 token。
- [ ] 新增 `backend/internal/handler/auth_feishu_oauth.go`：
  - `GET /api/v1/auth/oauth/feishu/start`
  - `GET /api/v1/auth/oauth/feishu/callback`
  - 如前端已有统一 OAuth pending 流程，飞书按现有 GitHub/Google/DingTalk 结构接入。
- [ ] 修改 `backend/internal/service/auth_service.go` 或现有 OAuth service：
  - `FindUserByAuthIdentity(provider=feishu, key=tenant_key, subject=open_id)`。
  - 未找到绑定且自动创建关闭时返回 `FEISHU_ACCOUNT_NOT_BOUND`。
  - 未找到绑定且自动创建开启时创建用户，应用 `feishu` 来源默认权益。
  - 登录成功后 upsert `feishu_user_profiles` 中该用户快照。
- [ ] 修改后端路由注册文件，把飞书 OAuth route 加入公开 auth routes。
- [ ] 修改 JWT/API Key 登录链路时不需要新增禁用判断，因为现有 `User.IsActive()` 已覆盖。补一条测试确认飞书禁用本地用户后 API Key 被拒绝。

### 验证和提交

- [ ] 运行飞书 handler 测试：

  ```bash
  cd backend && go test ./internal/handler -run 'TestFeishu'
  ```

- [ ] 运行认证相关测试：

  ```bash
  cd backend && go test ./internal/handler ./internal/service ./internal/server/middleware
  ```

- [ ] 提交：

  ```bash
  git add backend/internal/handler backend/internal/service backend/internal/server
  git commit -m "feat: add feishu oauth login"
  ```

---

## 任务 5：新增飞书组织同步 Ent schema 和迁移

### 先写测试

- [ ] 新增 `backend/internal/service/feishu_org_schema_test.go` 或放入组织服务测试文件：
  - `TestFeishuDepartmentUniqueTenantDepartment`
  - `TestFeishuUserProfileUniqueTenantOpenID`
  - `TestDepartmentGroupPolicyUniqueDepartmentGroup`
  - `TestDepartmentManagerUniqueManagerScope`
  - `TestUserGroupGrantUniqueUserGroupSource`
  - `TestFeishuOrgSyncRunStoresImpactSummary`
- [ ] 测试用 Ent sqlite 或项目现有 test DB helper 创建数据，断言唯一约束和 JSON 字段可读写。

运行预期失败：

```bash
cd backend && go test ./internal/service -run 'Test(FeishuDepartment|FeishuUserProfile|DepartmentGroupPolicy|DepartmentManager|UserGroupGrant|FeishuOrgSyncRun)'
```

### 实现

- [ ] 新增 `backend/ent/schema/feishu_department.go` 字段：
  - `tenant_key`
  - `department_id`
  - `parent_department_id`
  - `name`
  - `path`
  - `level`
  - `leader_user_ids`
  - `raw_data`
  - `synced_at`
  - 唯一索引 `(tenant_key, department_id)`
- [ ] 新增 `backend/ent/schema/feishu_user_profile.go` 字段：
  - `tenant_key`
  - `open_id`
  - `union_id`
  - `user_id_in_tenant`
  - `local_user_id`
  - `name`
  - `email`
  - `department_ids`
  - `primary_department_id`
  - `manager_open_id`
  - `status`
  - `in_app_scope`
  - `raw_data`
  - `last_seen_at`
  - `synced_at`
  - 唯一索引 `(tenant_key, open_id)`
  - 普通索引 `local_user_id`
- [ ] 新增 `backend/ent/schema/department_group_policy.go` 字段：
  - `tenant_key`
  - `department_id`
  - `group_id`
  - `inherit_to_children`
  - `enabled`
  - `created_by`
  - `updated_by`
  - 唯一索引 `(tenant_key, department_id, group_id)`
- [ ] 新增 `backend/ent/schema/department_manager.go` 字段：
  - `tenant_key`
  - `department_id`
  - `manager_open_id`
  - `manager_user_id`
  - `scope_mode`，值为 `self` 或 `subtree`
  - `source`
  - `synced_at`
  - 唯一索引 `(tenant_key, department_id, manager_open_id, scope_mode)`
- [ ] 新增 `backend/ent/schema/user_group_grant.go` 字段：
  - `user_id`
  - `group_id`
  - `source_type`，值为 `department_manager`、`department_policy`、`super_admin_override`
  - `source_id`
  - `tenant_key`
  - `department_id`
  - `granted_by`
  - `expires_at`
  - `created_at`
  - `updated_at`
  - 唯一索引 `(user_id, group_id, source_type, source_id)`
- [ ] 新增 `backend/ent/schema/org_permission_audit_log.go` 字段：
  - `actor_user_id`
  - `target_user_id`
  - `action`
  - `resource_type`
  - `resource_id`
  - `before_data`
  - `after_data`
  - `request_id`
  - `created_at`
- [ ] 新增 `backend/ent/schema/feishu_org_sync_run.go` 字段：
  - `tenant_key`
  - `status`
  - `started_at`
  - `finished_at`
  - `departments_seen`
  - `users_seen`
  - `bound_users`
  - `unbound_users`
  - `users_to_disable`
  - `review_required`
  - `blocked_reason`
  - `impact_summary`
  - `error_message`
- [ ] 运行 Ent 生成：

  ```bash
  cd backend && go generate ./ent
  ```

### 验证和提交

- [ ] 运行 schema 测试：

  ```bash
  cd backend && go test ./internal/service -run 'Test(FeishuDepartment|FeishuUserProfile|DepartmentGroupPolicy|DepartmentManager|UserGroupGrant|FeishuOrgSyncRun)'
  ```

- [ ] 运行后端编译：

  ```bash
  cd backend && go test ./...
  ```

- [ ] 提交：

  ```bash
  git add backend/ent
  git commit -m "feat: add feishu org permission schema"
  ```

---

## 任务 6：飞书组织同步服务、健康检查和影响预览

### 先写测试

- [ ] 新增 `backend/internal/service/feishu_org_service_test.go`：
  - `TestFeishuOrgSyncUpsertsDepartmentsAndUsers`
  - `TestFeishuOrgSyncMapsBoundUsersByAuthIdentity`
  - `TestFeishuOrgSyncDoesNotMatchByEmail`
  - `TestFeishuOrgSyncDisablesDepartedUser`
  - `TestFeishuOrgSyncBlocksLastActiveAdminDisable`
  - `TestFeishuOrgSyncRequiresReviewWhenDisableThresholdExceeded`
  - `TestFeishuOrgPreflightReportsScopeAndLastRun`
- [ ] 用 fake Feishu client 返回：
  - 一个 active 用户。
  - 一个 disabled 用户。
  - 一个不在可用范围内的用户。
  - 一个多部门用户。
  - 一个部门负责人。
- [ ] 测试断言：
  - sync 只 upsert 飞书镜像，不允许在本系统修改部门树。
  - 已绑定本地用户通过 `auth_identities` 映射。
  - email 相同但没有 `auth_identity` 时仍是 unbound。
  - 常规离职/禁用自动禁用本地用户并写 audit log。
  - 最后 active 超管保护阻断禁用。
  - 超阈值批量禁用写 `review_required=true`，不直接禁用。

运行预期失败：

```bash
cd backend && go test ./internal/service -run 'TestFeishuOrg'
```

### 实现

- [ ] 新增 `backend/internal/service/feishu_client.go`：
  - 定义接口 `FeishuClient`，包含获取 tenant token、用户列表、部门列表、用户详情、权限 scope 或健康检查所需方法。
  - HTTP 实现放在同文件或 `feishu_http_client.go`。
  - 测试只使用 fake client。
- [ ] 新增 `backend/internal/service/feishu_org_service.go`：
  - `SyncFeishuOrg(ctx, opts)`。
  - `PreviewFeishuOrgSync(ctx)`。
  - `CheckFeishuOrgHealth(ctx)`。
  - `ApplyPendingDisableReview(ctx, runID, adminID)`。
- [ ] 同步流程：
  - 读取飞书部门列表，upsert `feishu_departments`。
  - 读取飞书用户列表，upsert `feishu_user_profiles`。
  - 通过 `auth_identities` 映射本地用户。
  - 计算 `primary_department_id`，第一版使用飞书返回的第一个部门，并在多部门时写入提示字段。
  - upsert `department_managers`。
  - 计算离职/禁用/移出范围影响。
  - 通过批量保护后自动禁用本地用户。
  - 写 `feishu_org_sync_runs` 和 `org_permission_audit_logs`。
- [ ] 默认阈值：
  - `feishu_sync_disable_threshold_count=10`
  - `feishu_sync_disable_threshold_percent=20`
  - 任一条件超过即进入 review。
- [ ] 禁用用户时不要删除：
  - API Key
  - 余额
  - 订阅
  - 分组授权记录
  - 审计日志

### 验证和提交

- [ ] 运行组织同步测试：

  ```bash
  cd backend && go test ./internal/service -run 'TestFeishuOrg'
  ```

- [ ] 运行服务层测试：

  ```bash
  cd backend && go test ./internal/service
  ```

- [ ] 提交：

  ```bash
  git add backend/internal/service
  git commit -m "feat: sync feishu org mirror"
  ```

---

## 任务 7：组织权限服务和最终分组重算

### 先写测试

- [ ] 新增 `backend/internal/service/org_permission_service_test.go`：
  - `TestAdminAssignsDepartmentGroupPolicy`
  - `TestManagerGrantsGroupInsideManagedDepartment`
  - `TestManagerCannotGrantGroupOutsideDepartmentPool`
  - `TestManagerCannotManageEmployeeOutsideScope`
  - `TestManagerCannotRemoveAdminOverrideGrant`
  - `TestRecomputeUserAllowedGroupsIncludesDepartmentAndOverrideGrants`
  - `TestRecomputeUserAllowedGroupsRemovesRevokedManagerGrant`
- [ ] 测试数据：
  - 部门 `D1`，子部门 `D1-1`，部门 `D2`。
  - 超管授权 `D1` 可用分组 `G1`，授权 `D2` 可用分组 `G2`。
  - 负责人 `B` 管理 `D1`。
  - 员工 `A` 主部门 `D1-1`。
  - 员工 `C` 主部门 `D2`。
- [ ] 测试断言：
  - `B` 可给 `A` 授权 `G1`。
  - `B` 不可给 `A` 授权 `G2`。
  - `B` 不可给 `C` 授权任何分组。
  - 超管个人 override 不受 `B` 取消操作影响。

运行预期失败：

```bash
cd backend && go test ./internal/service -run 'Test(AdminAssignsDepartmentGroupPolicy|ManagerGrantsGroupInsideManagedDepartment|ManagerCannotGrantGroupOutsideDepartmentPool|ManagerCannotManageEmployeeOutsideScope|ManagerCannotRemoveAdminOverrideGrant|RecomputeUserAllowedGroups)'
```

### 实现

- [ ] 新增 `backend/internal/service/org_permission_service.go`：
  - `SetDepartmentGroupPolicies(ctx, adminID, departmentID, groupIDs, inheritToChildren)`。
  - `GrantUserGroupAsManager(ctx, managerID, userID, groupID)`。
  - `RevokeUserGroupAsManager(ctx, managerID, userID, groupID)`。
  - `GrantUserGroupAsAdminOverride(ctx, adminID, userID, groupID)`。
  - `RecomputeUserAllowedGroups(ctx, userID)`。
  - `ListManagerScope(ctx, managerID)`。
- [ ] `RecomputeUserAllowedGroups` 写入现有 `user_allowed_groups`：
  - 收集 active grant。
  - 合并去重 group id。
  - 调用现有 user repository 的 allowed groups 同步方法。
- [ ] 所有分配和撤销写 `org_permission_audit_logs`。
- [ ] 多部门提示：
  - 逻辑仍按 `primary_department_id` 判断。
  - 接口返回 `department_ids` 和 `primary_department_id`，前端展示“仅按主部门授权”。

### 验证和提交

- [ ] 运行权限服务测试：

  ```bash
  cd backend && go test ./internal/service -run 'Test(AdminAssignsDepartmentGroupPolicy|ManagerGrantsGroupInsideManagedDepartment|ManagerCannotGrantGroupOutsideDepartmentPool|ManagerCannotManageEmployeeOutsideScope|ManagerCannotRemoveAdminOverrideGrant|RecomputeUserAllowedGroups)'
  ```

- [ ] 运行用户分组相关测试：

  ```bash
  cd backend && go test ./internal/service ./internal/repository
  ```

- [ ] 提交：

  ```bash
  git add backend/internal/service backend/internal/repository
  git commit -m "feat: add org-scoped group grants"
  ```

---

## 任务 8：后台和负责人 API

### 先写测试

- [ ] 新增 `backend/internal/handler/admin/feishu_org_handler_test.go`：
  - `TestAdminListFeishuDepartments`
  - `TestAdminPreviewFeishuSync`
  - `TestAdminRunFeishuSync`
  - `TestAdminSetDepartmentGroupPolicy`
  - `TestAdminGrantUserOverrideGroup`
- [ ] 新增 `backend/internal/handler/org_manager_handler_test.go`：
  - `TestManagerListOwnDepartments`
  - `TestManagerListEmployeesInScope`
  - `TestManagerGrantGroup`
  - `TestManagerGrantGroupRejectsOutOfScopeEmployee`
  - `TestManagerGrantGroupRejectsOutOfPoolGroup`
- [ ] 测试断言：
  - 非超管无法调用 admin org API。
  - 非负责人无法调用 manager API。
  - 负责人 scope 变化后接口立即拒绝新的授权。

运行预期失败：

```bash
cd backend && go test ./internal/handler ./internal/handler/admin -run 'Test(Admin.*Feishu|Manager.*)'
```

### 实现

- [ ] 新增后台路由：
  - `GET /api/v1/admin/feishu/departments`
  - `GET /api/v1/admin/feishu/users`
  - `GET /api/v1/admin/feishu/sync/preview`
  - `POST /api/v1/admin/feishu/sync`
  - `GET /api/v1/admin/feishu/sync-runs`
  - `POST /api/v1/admin/feishu/sync-runs/:id/apply-review`
  - `PUT /api/v1/admin/org/departments/:department_id/groups`
  - `POST /api/v1/admin/users/:id/group-overrides`
  - `DELETE /api/v1/admin/users/:id/group-overrides/:group_id`
- [ ] 新增负责人路由：
  - `GET /api/v1/org-manager/departments`
  - `GET /api/v1/org-manager/employees`
  - `GET /api/v1/org-manager/employees/:id/groups`
  - `POST /api/v1/org-manager/employees/:id/groups`
  - `DELETE /api/v1/org-manager/employees/:id/groups/:group_id`
- [ ] 修改当前用户信息接口：
  - 返回 `is_org_manager`。
  - 返回 manager 入口所需的最小 scope 摘要。
- [ ] 响应结构包含：
  - `source_type`：`department_manager`、`department_policy`、`super_admin_override`。
  - `source_label`：页面显示用中文文案。
  - `primary_department_id` 和 `department_ids`。
  - `multi_department_notice`。

### 验证和提交

- [ ] 运行 handler 测试：

  ```bash
  cd backend && go test ./internal/handler ./internal/handler/admin -run 'Test(Admin.*Feishu|Manager.*)'
  ```

- [ ] 运行后端全量测试：

  ```bash
  make test-backend
  ```

- [ ] 提交：

  ```bash
  git add backend/internal/handler backend/internal/server backend/internal/service
  git commit -m "feat: expose feishu org permission APIs"
  ```

---

## 任务 9：前端类型、登录页和安全与认证设置页

### 先写测试

- [ ] 修改或新增前端测试：
  - `frontend/src/views/auth/__tests__/FeishuCallbackView.spec.ts`
  - `frontend/src/views/admin/__tests__/SettingsView.spec.ts`
  - `frontend/src/api/__tests__/auth.spec.ts`
- [ ] 测试用例：
  - `UserAuthProvider` 接受 `feishu`。
  - 飞书登录关闭时登录页不显示飞书入口。
  - 只启用飞书登录时飞书按钮是主操作。
  - 保存所有登录入口不可用时展示后端错误。
  - 飞书 App Secret 已配置时显示“已配置”，不回显密文。
  - `feishu` 来源默认权益区域可以编辑并提交。
  - 多部门用户展示“仅按主部门授权”提示。

运行预期失败：

```bash
pnpm --dir frontend exec vitest run src/views/auth/__tests__/FeishuCallbackView.spec.ts src/views/admin/__tests__/SettingsView.spec.ts src/api/__tests__/auth.spec.ts
```

### 实现

- [ ] 修改 `frontend/src/types/index.ts`：
  - `UserAuthProvider` 增加 `'feishu'`。
  - 设置类型增加 `feishu` 来源默认权益字段。
  - settings DTO 增加飞书登录配置和预检结果类型。
- [ ] 修改 `frontend/src/api/auth.ts`：
  - OAuth provider union 增加 `feishu`。
  - 新增飞书 OAuth start/callback API 或复用现有 generic OAuth helper。
- [ ] 新增或复用 auth callback view：
  - `frontend/src/views/auth/FeishuCallbackView.vue`
  - 失败时显示明确文案：未绑定本地账号、飞书配置未启用、租户不匹配、账号已禁用。
- [ ] 修改 `frontend/src/router/index.ts`：
  - 增加 `/auth/feishu/callback`。
  - 如果存在 email-completion 流程，飞书第一版不使用邮箱补全作为绑定手段。
- [ ] 修改登录页：
  - 按 `feishu_connect_enabled` 显示飞书按钮。
  - 按 `email_password_login_enabled` 显示邮箱密码表单。
  - 若所有入口都不可用，显示“当前没有可用登录方式，请联系管理员”。
- [ ] 修改 `frontend/src/views/admin/SettingsView.vue`：
  - 在“安全与认证”增加飞书登录卡片，布局参考钉钉登录。
  - 增加“检查配置”按钮，调用 `POST /api/v1/admin/settings/feishu/preflight`。
  - 增加登录入口开关：邮箱密码登录、超管邮箱备用登录、飞书登录。
  - 在来源默认权益里增加 `feishu`。
  - 保存失败时展示后端业务错误，不吞掉错误。
- [ ] 修改 `frontend/src/i18n/locales/zh.ts` 和 `frontend/src/i18n/locales/en.ts`：
  - 增加飞书登录、飞书配置、飞书默认权益、登录入口保护相关文案。

### 验证和提交

- [ ] 运行前端相关测试：

  ```bash
  pnpm --dir frontend exec vitest run src/views/auth/__tests__/FeishuCallbackView.spec.ts src/views/admin/__tests__/SettingsView.spec.ts src/api/__tests__/auth.spec.ts
  ```

- [ ] 运行关键前端测试：

  ```bash
  make test-frontend-critical
  ```

- [ ] 提交：

  ```bash
  git add frontend/src
  git commit -m "feat: add feishu login settings UI"
  ```

---

## 任务 10：前端组织权限页面和部门负责人页面

### 先写测试

- [ ] 新增测试：
  - `frontend/src/views/admin/__tests__/FeishuOrgView.spec.ts`
  - `frontend/src/views/org-manager/__tests__/OrgManagerView.spec.ts`
- [ ] 测试用例：
  - 超管页面展示飞书同步健康状态、最近同步时间、同步影响预览。
  - 超管可为部门选择分组池。
  - 超管可给单个员工添加或取消个人额外授权。
  - 部门负责人页面只展示自己负责范围内的员工。
  - 负责人只能看到员工所在部门可用分组。
  - 个人额外授权显示为不可由负责人取消。
  - 多部门员工显示“仅按主部门授权”。

运行预期失败：

```bash
pnpm --dir frontend exec vitest run src/views/admin/__tests__/FeishuOrgView.spec.ts src/views/org-manager/__tests__/OrgManagerView.spec.ts
```

### 实现

- [ ] 新增 `frontend/src/api/feishuOrg.ts`：
  - Admin API client。
  - Manager API client。
  - 类型定义和错误处理。
- [ ] 新增 `frontend/src/views/admin/FeishuOrgView.vue`：
  - 顶部状态区：飞书连接、组织同步、最近运行、影响预览。
  - 部门列表：部门名、路径、负责人、分组池。
  - 员工列表：姓名、本地用户绑定状态、主部门、多部门提示、当前可用分组来源。
  - 同步按钮：先预览，再确认执行。
  - 待确认禁用列表：只展示触发 review 的 run。
- [ ] 新增 `frontend/src/views/org-manager/OrgManagerView.vue`：
  - 负责人部门列表。
  - 员工列表。
  - 员工分组授权面板。
  - 禁用态和离职态用户不可分配。
- [ ] 修改导航：
  - 超管显示“飞书组织权限”入口。
  - `is_org_manager=true` 的普通用户显示“我的部门”入口。
- [ ] 页面设计约束：
  - 使用现有后台列表、筛选、开关、抽屉或弹窗样式。
  - 不做营销式大 hero。
  - 操作按钮用清晰图标和短文案。
  - 权限来源用 tag 展示：部门池、负责人授权、个人额外授权。

### 验证和提交

- [ ] 运行组织权限前端测试：

  ```bash
  pnpm --dir frontend exec vitest run src/views/admin/__tests__/FeishuOrgView.spec.ts src/views/org-manager/__tests__/OrgManagerView.spec.ts
  ```

- [ ] 运行前端构建：

  ```bash
  pnpm --dir frontend run build
  ```

- [ ] 提交：

  ```bash
  git add frontend/src
  git commit -m "feat: add feishu org permission UI"
  ```

---

## 任务 11：端到端冒烟和真实飞书应用验证

- [ ] 本地后端启动，使用本地配置连接测试飞书应用。
- [ ] 飞书开发者后台配置回调：

  ```text
  http://localhost:3000/oauth/feishu/callback
  ```

  如果本地前端端口不是 `3000`，以实际 Vite 端口为准，并同步修改飞书后台重定向 URL。

- [ ] 使用真实用户完成飞书登录，验证：
  - 首次未绑定时显示“未绑定本地账号，请联系管理员”。
  - 手动写入 `auth_identities` 后可登录同一个本地账号。
  - 飞书 email 为空不影响精确绑定登录。
  - `feishu_user_profiles` 写入 `open_id`、`union_id`、`tenant_key`、`department_ids`。
- [ ] 手动触发组织同步，验证：
  - 部门树只读展示。
  - 负责人来自飞书同步。
  - 多部门员工显示提示。
  - 影响预览和实际同步数量一致。
- [ ] 验证部门负责人权限：
  - 负责人可以给自己部门员工分配部门池内分组。
  - 负责人不能分配超管未授权给该部门的分组。
  - 负责人不能管理其他部门员工。
  - 负责人不能取消个人额外授权。
- [ ] 验证禁用策略：
  - 将测试飞书用户从可用范围移除或使用 fake 数据模拟，确认本地用户禁用。
  - 禁用后网页登录、JWT、API Key 都失败。
  - API Key、余额、订阅、授权记录没有被删除。
- [ ] 运行最终验证：

  ```bash
  make test
  make build
  ```

- [ ] 提交冒烟修复：

  ```bash
  git add backend frontend docs
  git commit -m "test: verify feishu org permission flow"
  ```

---

## 任务 12：收尾检查

- [ ] 检查没有密钥：

  ```bash
  make secret-scan
  ```

- [ ] 检查格式和空白：

  ```bash
  git diff --check
  ```

- [ ] 检查提交历史：

  ```bash
  git log --oneline --decorate -n 12
  ```

- [ ] 检查工作树：

  ```bash
  git status --short --branch
  ```

- [ ] 若 `tools/feishu-login-demo/` 仍需要保留，确认 `.env` 未被加入 git。
- [ ] 准备合并前说明：
  - 飞书登录如何开启。
  - 已有用户如何预写 `auth_identities`。
  - 飞书 scope 缺失时如何看健康检查。
  - 离职/禁用自动禁用和批量保护规则。
  - 部门负责人如何分配分组。
  - 多部门员工按主部门授权的限制。

---

## 执行顺序建议

1. 先完成任务 1 到 4，拿到可登录的飞书身份闭环。
2. 再完成任务 5 到 8，拿到组织同步和权限 API 闭环。
3. 再完成任务 9 到 10，补齐后台和负责人页面。
4. 最后完成任务 11 到 12，用真实飞书应用做一次手动验证。

第一版可以按这条边界上线：飞书登录、精确绑定、飞书来源默认权益、手动组织同步、部门池、负责人受限授权、个人额外授权、离职/禁用自动禁用。飞书事件回调、邮箱自动绑定、在本系统编辑组织架构、复杂多部门主部门选择器不进入第一版。
