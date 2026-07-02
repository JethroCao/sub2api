# Feishu Org Permissions Design

> **Status:** Draft spec for user review. Approved product direction from conversation: department group pools + department manager assignment + super-admin personal override.

## Goal

Add a Feishu/Lark internal-company identity and organization permission layer so company users can log in through a Feishu internal app, be mapped to the company org structure, and receive model group access through controlled department delegation.

## Non-Goals

- Do not add Volcengine Ark provider routing in this feature.
- Do not add payment or billing changes in this feature.
- Do not turn department managers into global admins.
- Do not replace the existing `user_allowed_groups` behavior for runtime group access; this feature adds source-aware grants and syncs effective access into the existing access path.

## Product Model

### Roles

- **Super Admin:** Existing `admin` role. Can configure Feishu, sync org data, assign department group pools, appoint department managers, and grant personal user overrides.
- **Department Manager:** A normal user with scoped org permissions. Can manage employees only inside departments they manage, and can assign only groups that the super admin allowed for those departments.
- **Employee:** A normal user. Uses API keys and model groups granted through department manager assignment, super-admin override, or existing public group behavior.

### Permission Rules

A department manager may grant or revoke group `G` for employee `A` only when all conditions hold:

1. The manager is active and is configured as a manager for `A`'s current primary department or one of its managed ancestor scopes.
2. `A` is active and belongs to a department the manager can manage.
3. `G` is active and belongs to the department's configured group pool.
4. The grant being modified was created by a department-manager source, not by super-admin override.

Super admins can grant and revoke any department policy, manager assignment, and personal override.

### Effective Group Access

Effective user groups are the union of:

- Existing non-exclusive public groups, unchanged.
- Existing direct `user_allowed_groups` rows.
- Department-manager grants.
- Super-admin personal overrides.

For compatibility with current API key and routing checks, source-aware grant changes must keep `user_allowed_groups` synchronized as the effective exclusive-group access list.

## Data Model

### `feishu_departments`

Stores Feishu organization departments.

- `id`
- `tenant_key`
- `department_id`
- `parent_department_id`
- `name`
- `path`
- `status`
- `synced_at`
- timestamps

Unique key: `(tenant_key, department_id)`.

### `feishu_user_profiles`

Maps local users to Feishu users and current org placement.

- `id`
- `user_id`
- `tenant_key`
- `open_id`
- `union_id`
- `user_id_in_tenant`
- `email`
- `name`
- `primary_department_id`
- `department_ids` as JSON array
- `status`
- `manager_open_id`
- `synced_at`
- timestamps

Unique keys: `(tenant_key, open_id)`, `(user_id)`.

### `department_group_policies`

Defines which groups a department manager is allowed to assign inside a department.

- `id`
- `department_id`
- `group_id`
- `enabled`
- `created_by_user_id`
- timestamps

Unique key: `(department_id, group_id)`.

### `department_managers`

Defines scoped managers for departments.

- `id`
- `department_id`
- `manager_user_id`
- `source` enum: `feishu`, `manual`
- `scope` enum: `department_only`, `include_subdepartments`
- `enabled`
- `created_by_user_id`
- timestamps

Unique key: `(department_id, manager_user_id)`.

### `user_group_grants`

Source-aware grants for individual users.

- `id`
- `user_id`
- `group_id`
- `source` enum: `department_manager`, `super_admin_override`
- `source_department_id`
- `granted_by_user_id`
- `revoked_at`
- timestamps

Active unique key: `(user_id, group_id, source, source_department_id)` where `revoked_at IS NULL`.

## Org Sync Rules

1. Feishu login creates or binds a local user through `auth_identities` with provider type `feishu`.
2. On login, fetch the Feishu user snapshot and update `feishu_user_profiles`.
3. Admin can run a manual org sync to refresh departments and selected users.
4. Later webhook sync can be added for user department changes and departures.
5. When a user's primary department changes, active `department_manager` grants from the previous department are revoked, and `super_admin_override` grants remain active.
6. When a Feishu user is disabled or removed, the local user should be disabled or marked for admin review according to a setting.

## Backend Design

### Auth

Add Feishu as a first-class OAuth provider, following the existing DingTalk/OIDC patterns:

- Add `feishu` to auth provider validators and frontend provider types.
- Add Feishu config keys for internal app credentials, redirect URL, tenant restriction, sync settings, and auto-provision behavior.
- Add `/api/v1/auth/oauth/feishu/start`, `/callback`, `/bind/start`, `/complete-registration`, `/create-account`, and `/bind-login`.
- Store canonical identity in `auth_identities` using `provider_type=feishu`, `provider_key=<tenant_key>`, and `provider_subject=<open_id or union_id>`.

### Org Permission Service

Create a focused service responsible for:

- Listing departments and department employees.
- Managing department group pools.
- Managing department managers.
- Managing source-aware user group grants.
- Recomputing effective `user_allowed_groups` after every grant/policy/org change.
- Checking whether the current user can mutate a target employee's department grant.

### Routes

Keep super-admin routes under `/api/v1/admin/feishu-org`.

Expose scoped department-manager routes outside global admin middleware, guarded by JWT + scoped org permission middleware:

- `GET /api/v1/org-manager/departments`
- `GET /api/v1/org-manager/departments/:id/users`
- `GET /api/v1/org-manager/departments/:id/groups`
- `PUT /api/v1/org-manager/users/:id/group-grants`

The scoped middleware must not use `AdminAuthMiddleware`, because that middleware requires `user.IsAdmin()` and would force managers into global admin.

## Frontend Design

### Super Admin

Add an "Org Permissions" admin page:

- Feishu connection status and sync action.
- Department tree with employee count and sync timestamp.
- Department group pool editor.
- Department manager editor.
- User detail drawer with personal override editor.
- Audit-friendly display of grant source tags: department manager, super-admin override.

### Department Manager

Add a scoped "My Department" page:

- Shows only departments the manager controls.
- Shows employees in those departments.
- Shows only assignable groups from each department pool.
- Allows grant/revoke for department-manager grants only.
- Displays super-admin override grants as read-only badges.

### Employee

In profile or key creation flows, show effective available groups without exposing admin-only controls. Source badges are useful but can be compact.

## UX Constraints

- Department managers should never see all groups by default.
- Department managers should never see users outside their managed department scope.
- Personal override must be visually distinct from department grants.
- When a user changes department, the UI should show which department grants were auto-revoked in audit/history.

## Testing Strategy

### Backend Unit Tests

- Manager cannot assign a group outside the department group pool.
- Manager cannot assign a group to a user outside managed departments.
- Manager cannot revoke a super-admin override.
- Super admin can create department policies and personal overrides.
- Department change revokes old department grants and preserves super-admin overrides.
- Effective group recomputation writes the expected `user_allowed_groups` rows.

### Backend Integration Tests

- Migrations create all org permission tables and indexes.
- Feishu auth identity can be bound without breaking existing providers.
- Existing admin user update still updates `user_allowed_groups`.
- API key auth cache is invalidated when effective groups change.

### Frontend Tests

- Admin org page limits group editor state correctly.
- Manager page renders only scoped departments/users/groups.
- Manager save request sends only department-manager grant changes.
- Read-only super-admin override badges cannot be toggled by managers.

## Rollout Plan

1. Add schema, migrations, service interfaces, and tests.
2. Add Feishu OAuth config and login/bind flow.
3. Add manual org sync and department/user read APIs.
4. Add department group pool and manager assignment APIs.
5. Add user grant APIs and effective `user_allowed_groups` recomputation.
6. Add admin org permissions UI.
7. Add department-manager UI and scoped routes.
8. Run backend and frontend tests; manually smoke Feishu login with a real internal app.

## Open Implementation Assumptions

- Primary department is the first Feishu department unless a later Feishu field gives a stronger primary-department signal.
- Department grants follow primary department for authorization; multi-department users can be supported later by allowing any active department membership.
- Department transfer auto-revokes old department-manager grants immediately; super-admin overrides remain.
- Initial release uses manual sync plus login-time user refresh; webhook sync can be a follow-up.

## Self-Review

- No unrelated Ark/payment work is included.
- Department manager is scoped, not global admin.
- Existing runtime group access stays compatible through `user_allowed_groups`.
- Source-aware grants distinguish department assignments from personal overrides.
- Employee transfer behavior is explicit.
