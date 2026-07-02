# 飞书组织权限设计

> **状态：** 待用户评审。当前方向来自前面对话：部门分组池 + 部门负责人分配 + 超管个人额外授权。

## 目标

为公司内部使用场景增加飞书/Lark 企业身份和组织权限层。公司员工通过飞书内部应用登录后，系统能识别员工所在部门、同步组织架构，并按部门授权规则给员工分配可用模型分组。

最终效果：

- 员工用飞书登录。
- 超管可以分别控制飞书登录、邮箱密码登录、开放注册。
- 组织架构从飞书同步，本系统不维护部门树、员工归属和上下级关系。
- 超管给部门配置“可分配的模型分组池”。
- 部门负责人只能给自己部门内员工分配部门池子里的组。
- 超管仍然可以给任意员工单独额外授权其他组。

## 不做什么

- 本功能不接火山方舟。
- 本功能不改支付和计费逻辑。
- 不把部门负责人变成全局 `admin`。
- 不在本系统维护组织架构；部门、员工归属、上下级和部门负责人都以飞书同步结果为准。
- 不直接替换现有 `user_allowed_groups` 运行时机制；本功能新增“带来源的授权记录”，再把最终有效权限同步到现有访问路径里。

## 产品模型

### 角色

- **超管：** 现有 `admin` 角色。可以配置飞书、触发组织架构同步、给部门配置分组池、给员工做个人额外授权。不能在本系统编辑部门树或员工归属。
- **部门负责人：** 普通用户，但拥有受限的组织管理权限。负责人身份来自飞书同步结果，只能管理自己负责部门里的员工，只能分配超管授权给该部门的分组。
- **员工：** 普通用户。使用 API Key 和模型分组，分组来源可以是部门负责人分配、超管个人额外授权，或现有公开分组规则。

### 权限规则

部门负责人 `B` 想给员工 `A` 分配或取消分组 `G`，必须同时满足：

1. `B` 是有效用户，并且根据飞书同步结果，是 `A` 所在主部门的负责人，或负责包含 `A` 所在部门的上级部门范围。
2. `A` 是有效用户，并且属于 `B` 可管理的部门范围。
3. `G` 是有效分组，并且属于 `A` 主部门被超管配置的分组池。
4. 本次修改的授权来源是“部门负责人分配”，不是“超管个人额外授权”。

超管不受这些限制。超管可以管理任意部门池和员工个人额外授权，但不在本系统编辑飞书组织架构。

### 员工最终可用分组

员工最终可用分组由以下来源合并：

- 现有非专属公开分组，保持不变。
- 从旧 `user_allowed_groups` 迁移导入的超管个人额外授权。
- 部门负责人分配的组。
- 超管个人额外授权的组。

为了兼容当前 API Key 和路由检查逻辑，所有带来源的授权变化都要重新计算员工的有效专属分组，并同步到现有 `user_allowed_groups` 表。

### 授权来源与旧入口兼容

第一版不再让后台用户编辑弹窗直接写入一份没有来源的 `user_allowed_groups`。超管在用户编辑弹窗或员工详情抽屉里修改专属分组时，统一写入 `user_group_grants.source=super_admin_override`，再由组织权限服务重算 `user_allowed_groups`。

兼容规则：

- 已存在的 `user_allowed_groups` 数据在迁移或首次重算时，按 `super_admin_override` 导入 `user_group_grants`。
- 导入后，`user_allowed_groups` 只作为运行时有效权限缓存，不作为新的人工维护入口。
- 部门负责人永远不能修改 `super_admin_override` 来源的授权。
- 超管可以在同一个员工详情里看到部门负责人分配、个人额外授权、公开分组三类结果，但只有个人额外授权可由超管直接编辑。

### 部门分组池有效范围

第一版按员工主部门判断可分配分组，不做隐式继承：

- 员工 `A` 的主部门是 `D`，部门负责人给 `A` 分配分组时，分组必须存在于 `D` 的部门分组池。
- 上级部门负责人可以管理子部门员工，但不能因此自动使用上级部门的分组池给子部门员工授权。
- 如果公司希望子部门使用上级部门的分组池，需要超管显式给子部门也配置相同分组。
- 后续可以增加“部门分组池继承”开关，但不放进第一版。

## 登录入口开关与安全策略

### 开关模型

新增或使用以下开关：

- `feishu_connect_enabled`：是否启用飞书登录。关闭后不显示飞书入口，后端飞书 OAuth 接口也拒绝请求。
- `email_password_login_enabled`：是否启用邮箱密码登录。关闭后普通用户不能用邮箱和密码登录。
- `admin_email_login_fallback_enabled`：邮箱密码登录关闭时，是否保留超管邮箱备用登录。默认建议开启，避免飞书配置异常时超管被锁在系统外。
- `registration_enabled`：沿用现有开放注册开关，控制新用户是否可自行注册。
- `feishu_connect_bypass_registration`：在飞书企业限制为 `internal_only` 时，是否允许本企业飞书用户绕过全局 `registration_enabled=false` 自动创建本地账号。

推荐的公司内配置：

```text
启用飞书登录：开
启用邮箱密码登录：关
保留超管邮箱备用登录：开
开放注册：关
飞书企业限制：仅本企业
允许本企业飞书用户绕过关闭注册自动创建账号：开
```

### 登录页显示规则

- `email_password_login_enabled=true` 时，显示现有邮箱密码表单。
- `email_password_login_enabled=false` 且 `admin_email_login_fallback_enabled=true` 时，不显示普通邮箱登录表单，只显示弱化的“管理员邮箱登录”入口。
- `email_password_login_enabled=false` 且 `admin_email_login_fallback_enabled=false` 时，完全隐藏邮箱密码登录入口。
- `feishu_connect_enabled=true` 时显示飞书登录入口。
- 当只启用飞书登录时，飞书按钮作为主登录动作，不需要显示“其他方式登录”的分割线。
- 注册入口只有在 `registration_enabled=true` 且 `email_password_login_enabled=true` 时才显示。
- 忘记密码入口只有在邮箱密码登录可用，或当前处于管理员邮箱备用登录界面时才显示。

### 后端拒绝规则

- `/api/v1/auth/login` 必须检查 `email_password_login_enabled`。
- 邮箱密码登录关闭且超管备用登录关闭时，`/api/v1/auth/login` 直接返回邮箱登录已关闭。
- 邮箱密码登录关闭但超管备用登录开启时，后端只允许 `role=admin` 的用户通过邮箱密码登录；普通用户即使密码正确也拒绝。
- `/api/v1/auth/register` 必须同时满足 `registration_enabled=true` 和 `email_password_login_enabled=true`，否则拒绝邮箱注册。
- `/api/v1/auth/oauth/feishu/*` 必须检查 `feishu_connect_enabled`。
- 飞书自动创建账号仅在 `feishu_connect_bypass_registration=true` 且企业限制策略为 `internal_only` 时允许绕过全局关闭注册。
- 后端 public settings 需要暴露 `email_password_login_enabled`、`admin_email_login_fallback_enabled`、`feishu_oauth_enabled`，供登录页决定显示状态。

## 数据模型

### `feishu_departments`

保存从飞书同步下来的部门信息。该表是只读镜像，不作为组织架构的维护入口。

- `id`
- `tenant_key`：飞书租户标识。
- `department_id`：飞书部门 ID。
- `parent_department_id`：父部门 ID。
- `name`：部门名。
- `path`：完整部门路径。
- `status`：部门状态。
- `synced_at`：最近同步时间。
- 创建和更新时间。

唯一键：`(tenant_key, department_id)`。

### `feishu_user_profiles`

保存本地用户和飞书用户、部门关系的映射。用户部门归属、直属上级和状态来自飞书同步，不在本系统手工编辑。

- `id`
- `user_id`：本地用户 ID。
- `tenant_key`
- `open_id`
- `union_id`
- `user_id_in_tenant`：飞书企业内用户 ID。
- `email`
- `name`
- `primary_department_id`：主部门 ID。
- `department_ids`：用户所属部门 ID 数组，使用 JSON 保存。
- `status`：飞书用户状态。
- `manager_open_id`：飞书侧直属上级。
- `synced_at`
- 创建和更新时间。

唯一键：`(tenant_key, open_id)`、`(user_id)`。

### `department_group_policies`

定义“某个部门可以分配哪些分组”。

- `id`
- `department_id`
- `group_id`
- `enabled`
- `created_by_user_id`：由哪个超管创建。
- 创建和更新时间。

唯一键：`(department_id, group_id)`。

### `department_managers`

保存从飞书同步出来的部门负责人或上级关系投影。该表只用于本系统权限判断，不用于维护组织架构。

- `id`
- `department_id`
- `manager_user_id`
- `feishu_relation_type`：`department_leader` 或 `manager_chain`。
- `scope`：`department_only` 或 `include_subdepartments`。
- `enabled`
- `synced_at`
- 创建和更新时间。

唯一键：`(department_id, manager_user_id)`。

### `user_group_grants`

保存带来源的员工分组授权。

- `id`
- `user_id`
- `group_id`
- `source`：`department_manager` 或 `super_admin_override`。
- `source_department_id`：如果是部门负责人分配，记录来源部门。
- `granted_by_user_id`：授权人。
- `revoked_at`：撤销时间，空表示当前有效。
- 创建和更新时间。

有效授权唯一键：`(user_id, group_id, source, source_department_id)`，仅对 `revoked_at IS NULL` 的记录生效。

### `org_permission_audit_logs`

保存组织权限相关的审计记录，方便排查“谁给谁开了什么组、为什么被撤销”。

- `id`
- `actor_user_id`：操作人。系统自动撤销时为空或使用系统用户。
- `target_user_id`：被授权或被撤销授权的员工。
- `group_id`
- `department_id`：授权发生时使用的部门上下文。
- `action`：`grant`、`revoke`、`auto_revoke`、`sync_import`。
- `source`：`department_manager` 或 `super_admin_override`。
- `reason`：如 `manual`、`department_changed`、`user_disabled`、`legacy_import`。
- `sync_run_id`：如果来自组织同步，记录对应同步批次。
- 创建时间。

审计日志只追加，不参与权限判断。

### `feishu_org_sync_runs`

保存飞书组织同步状态，用于后台健康检查和失败提示。

- `id`
- `status`：`running`、`success`、`partial_failed`、`failed`。
- `started_at`
- `finished_at`
- `departments_synced`
- `users_synced`
- `managers_synced`
- `error_message`
- `triggered_by_user_id`

同步状态用于展示，不直接决定用户能否继续使用已有权限。

## 组织同步规则

1. 飞书登录会创建或绑定本地用户，并在 `auth_identities` 中记录 `provider_type=feishu`。
2. 用户每次飞书登录时，刷新一次飞书用户快照，更新 `feishu_user_profiles`。
3. 超管可以手动触发组织同步，刷新部门和用户数据。
4. 同步结果覆盖本地组织镜像；本系统不提供部门新增、部门改名、员工换部门、负责人调整等维护入口。
5. 第一版先做手动同步和登录时同步；飞书事件回调可以后续再做。
6. 员工主部门变化时，旧部门来源的 `department_manager` 授权自动撤销；`super_admin_override` 保留。
7. 飞书用户离职、禁用或移除时，本地用户按配置自动禁用，或进入待超管确认状态。

### 同步失败和数据过期

- 同步完全失败时，不覆盖现有组织镜像，不自动撤销任何授权，只记录失败同步批次。
- 同步部分失败时，只提交已经完整校验的部门、用户和负责人数据；未完整校验的部分沿用旧镜像，并在同步批次里标记 `partial_failed`。
- 只有在同步成功确认员工主部门变化、离职、禁用或移除后，才执行部门来源授权的自动撤销。
- 最近一次成功同步超过 24 小时时，超管组织权限页和部门负责人页面显示“组织数据可能过期”的提示。
- 组织数据过期不阻断部门负责人操作，但保存授权时要在后端再次按当前镜像校验权限。

## 后端设计

### 飞书登录

把飞书作为一等 OAuth 登录方式接入，整体参考现有 DingTalk/OIDC 的结构：

- 在后端认证 provider 校验和前端 provider 类型中增加 `feishu`。
- 增加飞书配置项：内部应用凭证、回调地址、租户限制、同步选项、是否自动创建用户。
- 飞书登录区域参考钉钉登录区域，并包含 `feishu_connect_enabled` 总开关。
- 增加这些接口：
  - `GET /api/v1/auth/oauth/feishu/start`
  - `GET /api/v1/auth/oauth/feishu/callback`
  - `GET /api/v1/auth/oauth/feishu/bind/start`
  - `POST /api/v1/auth/oauth/feishu/complete-registration`
  - `POST /api/v1/auth/oauth/feishu/create-account`
  - `POST /api/v1/auth/oauth/feishu/bind-login`
- `auth_identities` 中使用：
  - `provider_type=feishu`
  - `provider_key=<tenant_key>`
  - `provider_subject=<open_id 或 union_id>`

### 邮箱密码登录

新增邮箱密码登录总开关，和现有注册开关解耦：

- `email_password_login_enabled=true`：保持现有邮箱密码登录行为。
- `email_password_login_enabled=false`：普通用户不能通过邮箱密码登录。
- `admin_email_login_fallback_enabled=true`：邮箱登录关闭时，仍允许超管用邮箱密码进入后台。

该开关不能只做前端隐藏。后端登录接口必须强制校验，否则用户仍可直接调用 `/api/v1/auth/login`。

### 组织权限服务

新增一个聚焦的组织权限服务，负责：

- 查询部门和部门员工。
- 管理部门分组池。
- 读取飞书同步出来的部门负责人或上级关系，用于 scoped 权限判断。
- 管理带来源的员工分组授权。
- 每次授权、部门策略或组织关系变化后，重新计算并同步 `user_allowed_groups`。
- 判断当前用户是否有权限修改某个员工的部门授权。
- 写入组织权限审计日志。
- 读取最近一次飞书组织同步状态，供后台展示健康检查。

### 路由设计

超管接口放在：

- `/api/v1/admin/feishu-org`

部门负责人接口不要走全局 admin middleware，而是使用 JWT + 组织权限中间件：

- `GET /api/v1/org-manager/departments`
- `GET /api/v1/org-manager/departments/:id/users`
- `GET /api/v1/org-manager/departments/:id/groups`
- `PUT /api/v1/org-manager/users/:id/group-grants`

这里不能复用 `AdminAuthMiddleware`，因为它要求 `user.IsAdmin()`，会迫使部门负责人变成全局管理员，权限会过大。

## 前端设计

### 超管后台

新增“组织权限”页面：

- 飞书连接状态、组织同步健康状态和同步按钮。
- 部门树，只读展示员工数量和最近同步时间。
- 部门分组池编辑器。
- 部门负责人只读展示，来源为飞书同步结果。
- 员工详情抽屉，支持个人额外授权。
- 授权来源标签：部门负责人分配、超管个人额外授权。
- 权限变更日志入口，可以按员工、部门、分组和来源筛选。

在“安全与认证”设置页补充：

- 邮箱密码登录卡片：启用邮箱密码登录、保留超管邮箱备用登录。
- 飞书登录卡片：启用飞书登录、App ID、App Secret、回调地址、企业限制策略、绕过关闭注册自动创建账号、同步姓名、同步企业邮箱、同步部门、同步组织架构。
- 飞书健康检查状态：未配置、已配置未启用、启用但回调异常、权限 scope 缺失、最近同步成功、最近同步失败。
- 登录入口预览或提示：当邮箱密码登录关闭且飞书登录未正确配置时，提示可能锁定普通员工登录。

### 部门负责人后台

新增“我的部门”页面：

- 只显示当前负责人可管理的部门。
- 只显示这些部门内员工。
- 只显示该部门池子里的可分配分组。
- 只能新增或撤销“部门负责人分配”的授权。
- 超管个人额外授权以只读标签展示，不能被部门负责人修改。
- 如果当前用户没有可管理部门，不显示左侧导航入口；直接访问页面时显示无权限空状态。
- 如果飞书同步后用户不再是部门负责人，下次刷新权限后隐藏入口，并拒绝保存新的部门授权。
- 部门负责人可以给自己分配部门池内分组，但仍然只能写入 `department_manager` 来源，不能给自己加超管个人额外授权。
- 当最近一次成功同步超过 24 小时时，页面顶部显示组织数据可能过期提示。

### 员工侧

在个人资料或 API Key 创建流程里，展示员工最终可用分组。来源标签可以做得紧凑，不需要暴露管理入口。

## 交互约束

- 部门负责人默认不能看到全量分组。
- 部门负责人不能看到自己管理范围外的员工。
- 超管个人额外授权必须和部门授权在视觉上区分清楚。
- 员工转部门时，界面和审计记录要能看到哪些部门授权被自动撤销。
- 部门分组池第一版不做隐式继承，界面文案要提示“仅作用于当前部门员工的主部门授权判断”。
- `user_allowed_groups` 在界面上只展示最终有效结果，编辑入口必须落到具体授权来源。
- 所有授权新增、撤销、自动撤销、旧数据导入都要写入审计日志。

## 测试策略

### 后端单元测试

- 部门负责人不能分配部门池外的组。
- 部门负责人不能给管理范围外的员工分配组。
- 部门负责人不能撤销超管个人额外授权。
- 上级部门负责人给子部门员工分配分组时，按员工主部门分组池判断，不能使用上级部门池隐式授权。
- 超管可以创建部门分组池和个人额外授权。
- 本系统不能新增、编辑或删除部门，也不能手工修改员工部门归属。
- 员工转部门会撤销旧部门授权，并保留超管个人额外授权。
- 同步完全失败时不覆盖旧组织镜像，也不自动撤销授权。
- 同步部分失败时只提交完整校验的数据，并保留旧镜像中未确认部分。
- 旧 `user_allowed_groups` 数据导入为 `super_admin_override` 后，运行时有效权限不丢失。
- 所有授权变更会写入审计日志。
- 重新计算有效分组后，`user_allowed_groups` 写入结果正确。
- 邮箱密码登录关闭时，普通用户不能通过 `/auth/login` 登录。
- 邮箱密码登录关闭但超管备用登录开启时，超管可以通过邮箱密码登录。
- 开放注册开启但邮箱密码登录关闭时，邮箱注册仍被拒绝。
- 飞书登录关闭时，飞书 OAuth start/callback/create-account/bind-login 接口都拒绝请求。
- 飞书 `internal_only` + bypass registration 开启时，本企业飞书用户可以在关闭注册的情况下自动创建账号。

### 后端集成测试

- 迁移能创建所有组织权限表和索引。
- 飞书身份能绑定到本地用户，不影响现有 provider。
- 现有超管更新用户专属分组时，会先写入 `super_admin_override`，再由组织权限服务重算 `user_allowed_groups`。
- 有效分组变化时，API Key auth cache 会失效。

### 前端测试

- 超管组织权限页面能正确限制部门分组池编辑状态。
- 超管组织权限页面只读展示部门树、员工归属和负责人关系。
- 超管组织权限页面能展示飞书配置健康状态、最近同步状态和组织数据过期提示。
- 超管用户编辑入口修改专属分组时，表现为个人额外授权，不直接编辑无来源 `user_allowed_groups`。
- 部门负责人页面只渲染权限范围内的部门、员工和分组。
- 无可管理部门的普通用户看不到“我的部门”入口，直接访问时看到无权限空状态。
- 部门负责人保存时，只发送部门负责人授权变化。
- 超管个人额外授权标签是只读的，不能被部门负责人切换。
- 登录页根据邮箱密码登录、超管备用登录、飞书登录开关显示正确入口。
- 只启用飞书登录时，飞书按钮作为主登录动作。

## 落地顺序

1. 增加 schema、迁移、服务接口和测试。
2. 增加登录入口开关：邮箱密码登录、超管邮箱备用登录、飞书登录开关。
3. 增加飞书 OAuth 配置和登录/绑定流程。
4. 增加登录页显示规则和 public settings 暴露。
5. 增加手动组织同步，以及部门/用户/负责人只读接口。
6. 增加同步健康状态、数据过期提示和同步失败保护。
7. 增加部门分组池接口，以及基于飞书负责人关系的权限判断。
8. 增加员工授权接口、旧 `user_allowed_groups` 导入、审计日志和有效权限重算。
9. 增加超管组织权限页面。
10. 增加部门负责人页面和 scoped routes。
11. 跑后端和前端测试；用真实飞书内部应用做一次手动冒烟测试。

## 实现假设

- 第一版把飞书返回的第一个部门当作主部门；如果后续飞书接口能提供更明确的主部门字段，再切换。
- 授权判断按主部门执行；多部门员工后续可以扩展为任意有效部门成员关系。
- 部门分组池第一版不做隐式继承；上级负责人能管子部门员工，但仍按员工主部门的分组池判断。
- 部门负责人和上下级关系来自飞书同步结果，本系统不维护组织架构。
- 员工转部门时，旧部门负责人发放的授权立即自动撤销；超管个人额外授权保留。
- 旧 `user_allowed_groups` 人工授权统一迁移为 `super_admin_override`，迁移后 `user_allowed_groups` 只保留为运行时有效权限缓存。
- 同步失败不会覆盖旧组织镜像，也不会触发自动撤销；只有成功确认的组织变化才会改变授权。
- 第一版使用手动同步 + 登录时刷新用户信息；飞书事件回调作为后续增强。

## 自查结论

- 没有包含火山方舟和支付改动。
- 部门负责人是 scoped manager，不是全局 admin。
- 当前运行时分组访问继续兼容 `user_allowed_groups`。
- 带来源授权能区分部门分配和个人额外授权。
- 员工转部门后的授权处理规则已经明确。
- 登录入口开关已经区分：邮箱密码登录、开放注册、飞书登录、超管备用登录。
- 组织架构明确以飞书为准，本系统只保存只读同步镜像和权限配置。
- Product Design 审阅中的 6 个风险点已经落到文档：旧入口兼容、部门池范围、同步失败策略、飞书健康检查、负责人空状态、审计日志。
