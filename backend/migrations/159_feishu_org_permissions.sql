CREATE TABLE IF NOT EXISTS feishu_departments (
    id BIGSERIAL PRIMARY KEY,
    tenant_key TEXT NOT NULL DEFAULT '',
    open_department_id TEXT NOT NULL,
    parent_open_department_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL DEFAULT '',
    leader_open_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    raw JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_synced_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT feishu_departments_status_check CHECK (status IN ('active', 'deleted'))
);

CREATE UNIQUE INDEX IF NOT EXISTS feishu_departments_tenant_open_department_key
    ON feishu_departments (tenant_key, open_department_id);

CREATE INDEX IF NOT EXISTS feishu_departments_parent_idx
    ON feishu_departments (tenant_key, parent_open_department_id);

CREATE INDEX IF NOT EXISTS feishu_departments_status_idx
    ON feishu_departments (status);

CREATE TABLE IF NOT EXISTS feishu_org_users (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    tenant_key TEXT NOT NULL DEFAULT '',
    open_id TEXT NOT NULL,
    union_id TEXT NOT NULL DEFAULT '',
    feishu_user_id TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT '',
    employee_no TEXT NOT NULL DEFAULT '',
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    primary_open_department_id TEXT NOT NULL DEFAULT '',
    manager_open_id TEXT NOT NULL DEFAULT '',
    department_open_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    raw JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_synced_at TIMESTAMPTZ NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT feishu_org_users_status_check CHECK (status IN ('active', 'disabled', 'departed'))
);

CREATE UNIQUE INDEX IF NOT EXISTS feishu_org_users_tenant_open_id_key
    ON feishu_org_users (tenant_key, open_id);

CREATE INDEX IF NOT EXISTS feishu_org_users_local_user_idx
    ON feishu_org_users (user_id);

CREATE INDEX IF NOT EXISTS feishu_org_users_union_id_idx
    ON feishu_org_users (union_id);

CREATE INDEX IF NOT EXISTS feishu_org_users_feishu_user_id_idx
    ON feishu_org_users (feishu_user_id);

CREATE INDEX IF NOT EXISTS feishu_org_users_status_idx
    ON feishu_org_users (status);

CREATE TABLE IF NOT EXISTS feishu_user_departments (
    tenant_key TEXT NOT NULL DEFAULT '',
    open_id TEXT NOT NULL,
    open_department_id TEXT NOT NULL,
    user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    is_primary BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_key, open_id, open_department_id)
);

CREATE INDEX IF NOT EXISTS feishu_user_departments_user_idx
    ON feishu_user_departments (user_id);

CREATE INDEX IF NOT EXISTS feishu_user_departments_department_idx
    ON feishu_user_departments (tenant_key, open_department_id);

CREATE TABLE IF NOT EXISTS feishu_department_managers (
    tenant_key TEXT NOT NULL DEFAULT '',
    open_department_id TEXT NOT NULL,
    manager_open_id TEXT NOT NULL,
    manager_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    source VARCHAR(20) NOT NULL DEFAULT 'feishu',
    relation_type VARCHAR(30) NOT NULL DEFAULT 'department_leader',
    include_subdepartments BOOLEAN NOT NULL DEFAULT true,
    status VARCHAR(20) NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_key, open_department_id, manager_open_id),
    CONSTRAINT feishu_department_managers_source_check CHECK (source IN ('feishu', 'manual')),
    CONSTRAINT feishu_department_managers_relation_check CHECK (relation_type IN ('department_leader', 'manager_chain')),
    CONSTRAINT feishu_department_managers_status_check CHECK (status IN ('active', 'disabled'))
);

CREATE INDEX IF NOT EXISTS feishu_department_managers_user_idx
    ON feishu_department_managers (manager_user_id);

CREATE INDEX IF NOT EXISTS feishu_department_managers_department_idx
    ON feishu_department_managers (tenant_key, open_department_id, status);

CREATE TABLE IF NOT EXISTS feishu_department_group_grants (
    tenant_key TEXT NOT NULL DEFAULT '',
    open_department_id TEXT NOT NULL,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    granted_by_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_key, open_department_id, group_id)
);

CREATE INDEX IF NOT EXISTS feishu_department_group_grants_group_idx
    ON feishu_department_group_grants (group_id);

CREATE TABLE IF NOT EXISTS feishu_user_group_grants (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    source VARCHAR(40) NOT NULL,
    source_open_department_id TEXT NOT NULL DEFAULT '',
    granted_by_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    reason TEXT NOT NULL DEFAULT '',
    revoked_at TIMESTAMPTZ NULL,
    revoked_by_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    revoke_reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT feishu_user_group_grants_source_check CHECK (source IN ('department_manager', 'super_admin_override'))
);

CREATE UNIQUE INDEX IF NOT EXISTS feishu_user_group_grants_active_unique
    ON feishu_user_group_grants (user_id, group_id, source, source_open_department_id)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS feishu_user_group_grants_user_idx
    ON feishu_user_group_grants (user_id, revoked_at);

CREATE INDEX IF NOT EXISTS feishu_user_group_grants_group_idx
    ON feishu_user_group_grants (group_id);

CREATE INDEX IF NOT EXISTS feishu_user_group_grants_source_department_idx
    ON feishu_user_group_grants (source, source_open_department_id)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS feishu_org_permission_audit_logs (
    id BIGSERIAL PRIMARY KEY,
    actor_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    target_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    group_id BIGINT NULL REFERENCES groups(id) ON DELETE SET NULL,
    tenant_key TEXT NOT NULL DEFAULT '',
    open_department_id TEXT NOT NULL DEFAULT '',
    action VARCHAR(40) NOT NULL,
    source VARCHAR(40) NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    sync_run_id BIGINT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT feishu_org_permission_audit_action_check CHECK (action IN ('grant', 'revoke', 'auto_revoke', 'sync_import', 'auto_disable_user', 'sync_blocked_for_review', 'recalculate'))
);

CREATE INDEX IF NOT EXISTS feishu_org_permission_audit_target_idx
    ON feishu_org_permission_audit_logs (target_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS feishu_org_permission_audit_department_idx
    ON feishu_org_permission_audit_logs (tenant_key, open_department_id, created_at DESC);

CREATE TABLE IF NOT EXISTS feishu_org_sync_runs (
    id BIGSERIAL PRIMARY KEY,
    status VARCHAR(20) NOT NULL DEFAULT 'running',
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ NULL,
    departments_synced INTEGER NOT NULL DEFAULT 0,
    users_synced INTEGER NOT NULL DEFAULT 0,
    managers_synced INTEGER NOT NULL DEFAULT 0,
    users_to_create INTEGER NOT NULL DEFAULT 0,
    users_to_disable INTEGER NOT NULL DEFAULT 0,
    bindings_missing INTEGER NOT NULL DEFAULT 0,
    review_required BOOLEAN NOT NULL DEFAULT false,
    error_message TEXT NOT NULL DEFAULT '',
    triggered_by_user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT feishu_org_sync_runs_status_check CHECK (status IN ('running', 'success', 'partial_failed', 'failed'))
);

CREATE INDEX IF NOT EXISTS feishu_org_sync_runs_status_idx
    ON feishu_org_sync_runs (status, started_at DESC);
