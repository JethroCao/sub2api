# 飞书组织权限设计

> **状态：** 待用户评审。当前方向来自前面对话：部门分组池 + 部门负责人分配 + 超管个人额外授权。

## 目标

为公司内部使用场景增加飞书/Lark 企业身份和组织权限层。公司员工通过飞书内部应用登录后，系统能识别员工所在部门、同步组织架构，并按部门授权规则给员工分配可用模型分组。

最终效果：

- 员工用飞书登录。
- 超管给部门配置“可分配的模型分组池”。
- 部门负责人只能给自己部门内员工分配部门池子里的组。
- 超管仍然可以给任意员工单独额外授权其他组。

## 不做什么

- 本功能不接火山方舟。
- 本功能不改支付和计费逻辑。
- 不把部门负责人变成全局 `admin`。
- 不直接替换现有 `user_allowed_groups` 运行时机制；本功能新增“带来源的授权记录”，再把最终有效权限同步到现有访问路径里。

## 产品模型

### 角色

- **超管：** 现有 `admin` 角色。可以配置飞书、同步组织架构、给部门配置分组池、设置部门负责人、给员工做个人额外授权。
- **部门负责人：** 普通用户，但拥有受限的组织管理权限。只能管理自己负责部门里的员工，只能分配超管授权给该部门的分组。
- **员工：** 普通用户。使用 API Key 和模型分组，分组来源可以是部门负责人分配、超管个人额外授权，或现有公开分组规则。

### 权限规则

部门负责人 `B` 想给员工 `A` 分配或取消分组 `G`，必须同时满足：

1. `B` 是有效用户，并且被配置为 `A` 所在主部门的负责人，或负责包含 `A` 所在部门的上级部门范围。
2. `A` 是有效用户，并且属于 `B` 可管理的部门范围。
3. `G` 是有效分组，并且属于该部门被超管配置的分组池。
4. 本次修改的授权来源是“部门负责人分配”，不是“超管个人额外授权”。

超管不受这些限制。超管可以管理任意部门池、负责人关系和员工个人额外授权。

### 员工最终可用分组

员工最终可用分组由以下来源合并：

- 现有非专属公开分组，保持不变。
- 现有直接写入的 `user_allowed_groups`。
- 部门负责人分配的组。
- 超管个人额外授权的组。

为了兼容当前 API Key 和路由检查逻辑，所有带来源的授权变化都要重新计算员工的有效专属分组，并同步到现有 `user_allowed_groups` 表。

## 数据模型

### `feishu_departments`

保存飞书部门信息。

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

保存本地用户和飞书用户、部门关系的映射。

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

定义部门负责人。

- `id`
- `department_id`
- `manager_user_id`
- `source`：`feishu` 或 `manual`。
- `scope`：`department_only` 或 `include_subdepartments`。
- `enabled`
- `created_by_user_id`
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

## 组织同步规则

1. 飞书登录会创建或绑定本地用户，并在 `auth_identities` 中记录 `provider_type=feishu`。
2. 用户每次飞书登录时，刷新一次飞书用户快照，更新 `feishu_user_profiles`。
3. 超管可以手动触发组织同步，刷新部门和用户数据。
4. 第一版先做手动同步和登录时同步；飞书事件回调可以后续再做。
5. 员工主部门变化时，旧部门来源的 `department_manager` 授权自动撤销；`super_admin_override` 保留。
6. 飞书用户离职、禁用或移除时，本地用户按配置自动禁用，或进入待超管确认状态。

## 后端设计

### 飞书登录

把飞书作为一等 OAuth 登录方式接入，整体参考现有 DingTalk/OIDC 的结构：

- 在后端认证 provider 校验和前端 provider 类型中增加 `feishu`。
- 增加飞书配置项：内部应用凭证、回调地址、租户限制、同步选项、是否自动创建用户。
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

### 组织权限服务

新增一个聚焦的组织权限服务，负责：

- 查询部门和部门员工。
- 管理部门分组池。
- 管理部门负责人。
- 管理带来源的员工分组授权。
- 每次授权、部门策略或组织关系变化后，重新计算并同步 `user_allowed_groups`。
- 判断当前用户是否有权限修改某个员工的部门授权。

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

- 飞书连接状态和同步按钮。
- 部门树，展示员工数量和最近同步时间。
- 部门分组池编辑器。
- 部门负责人编辑器。
- 员工详情抽屉，支持个人额外授权。
- 授权来源标签：部门负责人分配、超管个人额外授权。

### 部门负责人后台

新增“我的部门”页面：

- 只显示当前负责人可管理的部门。
- 只显示这些部门内员工。
- 只显示该部门池子里的可分配分组。
- 只能新增或撤销“部门负责人分配”的授权。
- 超管个人额外授权以只读标签展示，不能被部门负责人修改。

### 员工侧

在个人资料或 API Key 创建流程里，展示员工最终可用分组。来源标签可以做得紧凑，不需要暴露管理入口。

## 交互约束

- 部门负责人默认不能看到全量分组。
- 部门负责人不能看到自己管理范围外的员工。
- 超管个人额外授权必须和部门授权在视觉上区分清楚。
- 员工转部门时，界面和审计记录要能看到哪些部门授权被自动撤销。

## 测试策略

### 后端单元测试

- 部门负责人不能分配部门池外的组。
- 部门负责人不能给管理范围外的员工分配组。
- 部门负责人不能撤销超管个人额外授权。
- 超管可以创建部门分组池和个人额外授权。
- 员工转部门会撤销旧部门授权，并保留超管个人额外授权。
- 重新计算有效分组后，`user_allowed_groups` 写入结果正确。

### 后端集成测试

- 迁移能创建所有组织权限表和索引。
- 飞书身份能绑定到本地用户，不影响现有 provider。
- 现有超管更新用户时，仍然能更新 `user_allowed_groups`。
- 有效分组变化时，API Key auth cache 会失效。

### 前端测试

- 超管组织权限页面能正确限制部门分组池编辑状态。
- 部门负责人页面只渲染权限范围内的部门、员工和分组。
- 部门负责人保存时，只发送部门负责人授权变化。
- 超管个人额外授权标签是只读的，不能被部门负责人切换。

## 落地顺序

1. 增加 schema、迁移、服务接口和测试。
2. 增加飞书 OAuth 配置和登录/绑定流程。
3. 增加手动组织同步，以及部门/用户只读接口。
4. 增加部门分组池和部门负责人管理接口。
5. 增加员工授权接口，以及 `user_allowed_groups` 有效权限重算。
6. 增加超管组织权限页面。
7. 增加部门负责人页面和 scoped routes。
8. 跑后端和前端测试；用真实飞书内部应用做一次手动冒烟测试。

## 实现假设

- 第一版把飞书返回的第一个部门当作主部门；如果后续飞书接口能提供更明确的主部门字段，再切换。
- 授权判断按主部门执行；多部门员工后续可以扩展为任意有效部门成员关系。
- 员工转部门时，旧部门负责人发放的授权立即自动撤销；超管个人额外授权保留。
- 第一版使用手动同步 + 登录时刷新用户信息；飞书事件回调作为后续增强。

## 自查结论

- 没有包含火山方舟和支付改动。
- 部门负责人是 scoped manager，不是全局 admin。
- 当前运行时分组访问继续兼容 `user_allowed_groups`。
- 带来源授权能区分部门分配和个人额外授权。
- 员工转部门后的授权处理规则已经明确。
