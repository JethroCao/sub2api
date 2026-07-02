package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	FeishuGrantSourceDepartmentManager = "department_manager"
	FeishuGrantSourceSuperAdmin        = "super_admin_override"
)

type FeishuOrgPermissionSnapshot struct {
	ManagerDepartmentIDs         []string
	EmployeePrimaryDepartmentID  string
	EmployeeDepartmentIDs        []string
	DepartmentAssignableGroupIDs map[string][]int64
	DepartmentManagerGroupGrants []int64
	UserOverrideGroupGrants      []int64
}

func (s FeishuOrgPermissionSnapshot) ManagerAssignableGroupIDs() []int64 {
	deptID := strings.TrimSpace(s.EmployeePrimaryDepartmentID)
	if deptID == "" {
		deptID = firstString(s.EmployeeDepartmentIDs)
	}
	if deptID == "" || !containsString(s.ManagerDepartmentIDs, deptID) {
		return nil
	}
	return uniqueSortedInt64s(s.DepartmentAssignableGroupIDs[deptID])
}

func (s FeishuOrgPermissionSnapshot) CanManagerAssignGroup(groupID int64) bool {
	if groupID <= 0 {
		return false
	}
	for _, candidate := range s.ManagerAssignableGroupIDs() {
		if candidate == groupID {
			return true
		}
	}
	return false
}

func (s FeishuOrgPermissionSnapshot) EffectiveEmployeeGroupIDs() []int64 {
	ids := append([]int64{}, s.DepartmentManagerGroupGrants...)
	ids = append(ids, s.UserOverrideGroupGrants...)
	return uniqueSortedInt64s(ids)
}

type feishuOrgSQLExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type FeishuOrgPermissionService struct {
	db                   feishuOrgSQLExecutor
	authCacheInvalidator APIKeyAuthCacheInvalidator
}

func NewFeishuOrgPermissionService(db *sql.DB, invalidator APIKeyAuthCacheInvalidator) *FeishuOrgPermissionService {
	return &FeishuOrgPermissionService{
		db:                   db,
		authCacheInvalidator: invalidator,
	}
}

type FeishuSetUserGroupGrantsInput struct {
	ActorUserID            int64
	TargetUserID           int64
	Source                 string
	SourceOpenDepartmentID string
	GroupIDs               []int64
	Reason                 string
}

type FeishuUserGroupGrantResult struct {
	UserID                 int64   `json:"user_id"`
	Source                 string  `json:"source"`
	SourceOpenDepartmentID string  `json:"source_open_department_id,omitempty"`
	GroupIDs               []int64 `json:"group_ids"`
}

type FeishuDepartmentManagerAssignmentInput struct {
	ManagerUserID int64
	TargetUserID  int64
	GroupIDs      []int64
	Reason        string
}

type FeishuDepartmentManagerAssignmentResult struct {
	FeishuUserGroupGrantResult
	TenantKey          string  `json:"tenant_key"`
	AssignableGroupIDs []int64 `json:"assignable_group_ids"`
}

type FeishuSetDepartmentGroupPoolInput struct {
	ActorUserID      int64
	TenantKey        string
	OpenDepartmentID string
	GroupIDs         []int64
	Reason           string
}

type FeishuDepartmentGroupPoolResult struct {
	TenantKey        string  `json:"tenant_key"`
	OpenDepartmentID string  `json:"open_department_id"`
	GroupIDs         []int64 `json:"group_ids"`
	RevokedUserIDs   []int64 `json:"revoked_user_ids,omitempty"`
}

type FeishuOrgListInput struct {
	TenantKey string
	Limit     int
	Offset    int
}

type FeishuOrgGroupBrief struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Platform         string `json:"platform"`
	SubscriptionType string `json:"subscription_type"`
}

type FeishuOrgDepartmentView struct {
	TenantKey              string                `json:"tenant_key"`
	OpenDepartmentID       string                `json:"open_department_id"`
	ParentOpenDepartmentID string                `json:"parent_open_department_id"`
	Name                   string                `json:"name"`
	Path                   string                `json:"path"`
	Status                 string                `json:"status"`
	LeaderOpenIDs          []string              `json:"leader_open_ids"`
	EmployeeCount          int64                 `json:"employee_count"`
	ManagerCount           int64                 `json:"manager_count"`
	AssignableGroups       []FeishuOrgGroupBrief `json:"assignable_groups"`
	LastSyncedAt           *time.Time            `json:"last_synced_at,omitempty"`
}

type FeishuOrgDepartmentListResult struct {
	Items  []FeishuOrgDepartmentView `json:"items"`
	Limit  int                       `json:"limit"`
	Offset int                       `json:"offset"`
}

type FeishuOrgUserView struct {
	UserID                     int64                 `json:"user_id"`
	LocalEmail                 string                `json:"local_email"`
	LocalUsername              string                `json:"local_username"`
	LocalStatus                string                `json:"local_status"`
	TenantKey                  string                `json:"tenant_key"`
	OpenID                     string                `json:"open_id"`
	UnionID                    string                `json:"union_id"`
	FeishuUserID               string                `json:"feishu_user_id"`
	Name                       string                `json:"name"`
	Email                      string                `json:"email"`
	EmployeeNo                 string                `json:"employee_no"`
	Status                     string                `json:"status"`
	PrimaryOpenDepartmentID    string                `json:"primary_open_department_id"`
	PrimaryDepartmentName      string                `json:"primary_department_name"`
	PrimaryDepartmentPath      string                `json:"primary_department_path"`
	DepartmentOpenIDs          []string              `json:"department_open_ids"`
	DepartmentManagerGroupIDs  []int64               `json:"department_manager_group_ids"`
	SuperAdminOverrideGroupIDs []int64               `json:"super_admin_override_group_ids"`
	EffectiveGroupIDs          []int64               `json:"effective_group_ids"`
	AssignableGroups           []FeishuOrgGroupBrief `json:"assignable_groups,omitempty"`
	LastSyncedAt               *time.Time            `json:"last_synced_at,omitempty"`
}

type FeishuOrgUserListResult struct {
	Items  []FeishuOrgUserView `json:"items"`
	Limit  int                 `json:"limit"`
	Offset int                 `json:"offset"`
}

type FeishuOrgSyncRunView struct {
	ID                int64      `json:"id"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	DepartmentsSynced int        `json:"departments_synced"`
	UsersSynced       int        `json:"users_synced"`
	ManagersSynced    int        `json:"managers_synced"`
	UsersToCreate     int        `json:"users_to_create"`
	UsersToDisable    int        `json:"users_to_disable"`
	BindingsMissing   int        `json:"bindings_missing"`
	ReviewRequired    bool       `json:"review_required"`
	ErrorMessage      string     `json:"error_message"`
	TriggeredByUserID int64      `json:"triggered_by_user_id"`
}

type FeishuOrgSyncRunListResult struct {
	Items  []FeishuOrgSyncRunView `json:"items"`
	Limit  int                    `json:"limit"`
	Offset int                    `json:"offset"`
}

type FeishuManualReconcileResult struct {
	SyncRun  FeishuOrgSyncRunView    `json:"sync_run"`
	Decision FeishuDepartureDecision `json:"decision"`
}

func (s *FeishuOrgPermissionService) ListDepartments(ctx context.Context, input FeishuOrgListInput) (*FeishuOrgDepartmentListResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	tenantKey, limit, offset := normalizeFeishuOrgListInput(input)
	rows, err := s.db.QueryContext(ctx, `
SELECT d.tenant_key,
       d.open_department_id,
       d.parent_open_department_id,
       d.name,
       d.path,
       d.status,
       d.leader_open_ids::text AS leader_open_ids,
       d.last_synced_at,
       COALESCE(employee_counts.employee_count, 0) AS employee_count,
       COALESCE(manager_counts.manager_count, 0) AS manager_count,
       COALESCE(group_pools.assignable_groups, '[]'::jsonb)::text AS assignable_groups
FROM feishu_departments d
LEFT JOIN (
    SELECT tenant_key, primary_open_department_id AS open_department_id, COUNT(*) AS employee_count
    FROM feishu_org_users
    WHERE status = 'active'
    GROUP BY tenant_key, primary_open_department_id
) employee_counts
  ON employee_counts.tenant_key = d.tenant_key
 AND employee_counts.open_department_id = d.open_department_id
LEFT JOIN (
    SELECT tenant_key, open_department_id, COUNT(*) AS manager_count
    FROM feishu_department_managers
    WHERE status = 'active'
    GROUP BY tenant_key, open_department_id
) manager_counts
  ON manager_counts.tenant_key = d.tenant_key
 AND manager_counts.open_department_id = d.open_department_id
LEFT JOIN (
    SELECT grants.tenant_key,
           grants.open_department_id,
           jsonb_agg(
             jsonb_build_object(
               'id', groups.id,
               'name', groups.name,
               'platform', groups.platform,
               'subscription_type', groups.subscription_type
             )
             ORDER BY groups.sort_order, groups.id
           ) AS assignable_groups
    FROM feishu_department_group_grants grants
    JOIN groups
      ON groups.id = grants.group_id
     AND groups.deleted_at IS NULL
    GROUP BY grants.tenant_key, grants.open_department_id
) group_pools
  ON group_pools.tenant_key = d.tenant_key
 AND group_pools.open_department_id = d.open_department_id
WHERE ($1 = '' OR d.tenant_key = $1)
  AND d.status = 'active'
ORDER BY d.path, d.name, d.open_department_id
LIMIT $2 OFFSET $3`, tenantKey, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]FeishuOrgDepartmentView, 0)
	for rows.Next() {
		var item FeishuOrgDepartmentView
		var leaderRaw string
		var groupsRaw string
		var lastSyncedAt sql.NullTime
		if err := rows.Scan(
			&item.TenantKey,
			&item.OpenDepartmentID,
			&item.ParentOpenDepartmentID,
			&item.Name,
			&item.Path,
			&item.Status,
			&leaderRaw,
			&lastSyncedAt,
			&item.EmployeeCount,
			&item.ManagerCount,
			&groupsRaw,
		); err != nil {
			return nil, err
		}
		leaders, err := decodeFeishuJSONStringSlice(leaderRaw)
		if err != nil {
			return nil, fmt.Errorf("decode department leaders: %w", err)
		}
		groups, err := decodeFeishuJSONGroups(groupsRaw)
		if err != nil {
			return nil, fmt.Errorf("decode department group pool: %w", err)
		}
		item.LeaderOpenIDs = leaders
		item.AssignableGroups = groups
		item.LastSyncedAt = sqlNullTimePtr(lastSyncedAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &FeishuOrgDepartmentListResult{Items: items, Limit: limit, Offset: offset}, nil
}

func (s *FeishuOrgPermissionService) ListUsers(ctx context.Context, input FeishuOrgListInput) (*FeishuOrgUserListResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	tenantKey, limit, offset := normalizeFeishuOrgListInput(input)
	rows, err := s.db.QueryContext(ctx, feishuOrgUsersListSQL(`
WHERE ($1 = '' OR fu.tenant_key = $1)
ORDER BY fu.name, fu.open_id
LIMIT $2 OFFSET $3`), tenantKey, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items, err := scanFeishuOrgUserViews(rows)
	if err != nil {
		return nil, err
	}
	return &FeishuOrgUserListResult{Items: items, Limit: limit, Offset: offset}, nil
}

func (s *FeishuOrgPermissionService) ListSyncRuns(ctx context.Context, input FeishuOrgListInput) (*FeishuOrgSyncRunListResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	_, limit, offset := normalizeFeishuOrgListInput(input)
	rows, err := s.db.QueryContext(ctx, `
SELECT id,
       status,
       started_at,
       finished_at,
       departments_synced,
       users_synced,
       managers_synced,
       users_to_create,
       users_to_disable,
       bindings_missing,
       review_required,
       error_message,
       triggered_by_user_id
FROM feishu_org_sync_runs
ORDER BY started_at DESC, id DESC
LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	items := make([]FeishuOrgSyncRunView, 0)
	for rows.Next() {
		var item FeishuOrgSyncRunView
		var finishedAt sql.NullTime
		var triggeredBy sql.NullInt64
		if err := rows.Scan(
			&item.ID,
			&item.Status,
			&item.StartedAt,
			&finishedAt,
			&item.DepartmentsSynced,
			&item.UsersSynced,
			&item.ManagersSynced,
			&item.UsersToCreate,
			&item.UsersToDisable,
			&item.BindingsMissing,
			&item.ReviewRequired,
			&item.ErrorMessage,
			&triggeredBy,
		); err != nil {
			return nil, err
		}
		item.FinishedAt = sqlNullTimePtr(finishedAt)
		if triggeredBy.Valid {
			item.TriggeredByUserID = triggeredBy.Int64
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &FeishuOrgSyncRunListResult{Items: items, Limit: limit, Offset: offset}, nil
}

func (s *FeishuOrgPermissionService) RunManualReconcile(ctx context.Context, actorUserID int64, policy FeishuDeparturePolicy) (*FeishuManualReconcileResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	previousActiveUsers, err := s.countActiveFeishuOrgUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("count active feishu org users: %w", err)
	}
	departedUserIDs, err := s.listDepartedActiveLocalUserIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list departed local users: %w", err)
	}
	decision := EvaluateFeishuDepartureDecision(previousActiveUsers, departedUserIDs, policy)

	status := "success"
	reviewRequired := false
	errorMessage := ""
	if decision.RequiresReview {
		status = "partial_failed"
		reviewRequired = true
		errorMessage = decision.Reason
		if err := s.appendAuditLog(ctx, actorUserID, 0, "", "", "sync_blocked_for_review", decision.Reason); err != nil {
			return nil, fmt.Errorf("append review audit log: %w", err)
		}
	} else if decision.AutoDisable {
		if err := s.disableLocalUsers(ctx, decision.UserIDs); err != nil {
			return nil, fmt.Errorf("disable local users: %w", err)
		}
		for _, userID := range decision.UserIDs {
			if err := s.appendAuditLog(ctx, actorUserID, userID, "", "", "auto_disable_user", "feishu_departure_auto_disable"); err != nil {
				return nil, fmt.Errorf("append auto disable audit log: %w", err)
			}
			if s.authCacheInvalidator != nil {
				s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
			}
		}
	}

	run, err := s.insertSyncRun(ctx, status, len(departedUserIDs), reviewRequired, errorMessage, actorUserID)
	if err != nil {
		return nil, fmt.Errorf("insert sync run: %w", err)
	}
	return &FeishuManualReconcileResult{SyncRun: *run, Decision: decision}, nil
}

func (s *FeishuOrgPermissionService) ListManagerUsers(ctx context.Context, managerUserID int64, input FeishuOrgListInput) (*FeishuOrgUserListResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	if managerUserID <= 0 {
		return nil, errors.New("manager_user_id is required")
	}
	_, limit, offset := normalizeFeishuOrgListInput(input)
	rows, err := s.db.QueryContext(ctx, `
WITH RECURSIVE manager_roots AS (
    SELECT tenant_key, open_department_id, include_subdepartments
    FROM feishu_department_managers
    WHERE manager_user_id = $1
      AND status = 'active'
),
managed_scope AS (
    SELECT tenant_key, open_department_id, include_subdepartments
    FROM manager_roots
    UNION ALL
    SELECT child.tenant_key, child.open_department_id, scope.include_subdepartments
    FROM feishu_departments child
    JOIN managed_scope scope
      ON child.tenant_key = scope.tenant_key
     AND child.parent_open_department_id = scope.open_department_id
    WHERE scope.include_subdepartments = true
      AND child.status = 'active'
)
`+feishuOrgUsersListSQL(`
JOIN managed_scope scope
  ON scope.tenant_key = fu.tenant_key
 AND scope.open_department_id = fu.primary_open_department_id
WHERE fu.status = 'active'
ORDER BY fu.name, fu.open_id
LIMIT $2 OFFSET $3`), managerUserID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items, err := scanFeishuOrgUserViews(rows)
	if err != nil {
		return nil, err
	}
	return &FeishuOrgUserListResult{Items: items, Limit: limit, Offset: offset}, nil
}

func (s *FeishuOrgPermissionService) countActiveFeishuOrgUsers(ctx context.Context) (int, error) {
	var count int
	err := scanFeishuSingleRow(ctx, s.db, `
SELECT COUNT(*) FROM feishu_org_users
WHERE user_id IS NOT NULL
  AND status = 'active'`, nil, &count)
	return count, err
}

func (s *FeishuOrgPermissionService) listDepartedActiveLocalUserIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT org_user.user_id
FROM feishu_org_users org_user
JOIN users local_user
  ON local_user.id = org_user.user_id
WHERE org_user.user_id IS NOT NULL
  AND org_user.status IN ('departed', 'disabled')
  AND local_user.status = $1
  AND local_user.role <> $2
ORDER BY org_user.user_id`, StatusActive, RoleAdmin)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedInt64s(ids), nil
}

func (s *FeishuOrgPermissionService) disableLocalUsers(ctx context.Context, userIDs []int64) error {
	userIDs = uniqueSortedInt64s(userIDs)
	if len(userIDs) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE users
SET status = $1,
    updated_at = NOW()
WHERE id = ANY($2)
  AND role <> $3`, StatusDisabled, pq.Array(userIDs), RoleAdmin)
	return err
}

func (s *FeishuOrgPermissionService) insertSyncRun(ctx context.Context, status string, usersToDisable int, reviewRequired bool, errorMessage string, actorUserID int64) (*FeishuOrgSyncRunView, error) {
	var item FeishuOrgSyncRunView
	var finishedAt sql.NullTime
	var triggeredBy sql.NullInt64
	err := scanFeishuSingleRow(ctx, s.db, `
INSERT INTO feishu_org_sync_runs (
    status,
    started_at,
    finished_at,
    users_to_disable,
    review_required,
    error_message,
    triggered_by_user_id
)
VALUES ($1, NOW(), NOW(), $2, $3, $4, NULLIF($5, 0))
RETURNING id,
          status,
          started_at,
          finished_at,
          departments_synced,
          users_synced,
          managers_synced,
          users_to_create,
          users_to_disable,
          bindings_missing,
          review_required,
          error_message,
          triggered_by_user_id`, []any{status, usersToDisable, reviewRequired, errorMessage, actorUserID},
		&item.ID,
		&item.Status,
		&item.StartedAt,
		&finishedAt,
		&item.DepartmentsSynced,
		&item.UsersSynced,
		&item.ManagersSynced,
		&item.UsersToCreate,
		&item.UsersToDisable,
		&item.BindingsMissing,
		&item.ReviewRequired,
		&item.ErrorMessage,
		&triggeredBy,
	)
	if err != nil {
		return nil, err
	}
	item.FinishedAt = sqlNullTimePtr(finishedAt)
	if triggeredBy.Valid {
		item.TriggeredByUserID = triggeredBy.Int64
	}
	return &item, nil
}

func (s *FeishuOrgPermissionService) SetDepartmentGroupPool(ctx context.Context, input FeishuSetDepartmentGroupPoolInput) (*FeishuDepartmentGroupPoolResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	tenantKey := strings.TrimSpace(input.TenantKey)
	if tenantKey == "" {
		return nil, errors.New("tenant_key is required")
	}
	deptID := strings.TrimSpace(input.OpenDepartmentID)
	if deptID == "" {
		return nil, errors.New("open_department_id is required")
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		reason = "pool_updated"
	}
	groupIDs := uniqueSortedInt64s(input.GroupIDs)

	affectedUserIDs, err := s.listUsersWithInvalidDepartmentManagerGrants(ctx, deptID, groupIDs)
	if err != nil {
		return nil, fmt.Errorf("list invalid department manager grants: %w", err)
	}
	if err := s.revokeInvalidDepartmentManagerGrants(ctx, deptID, groupIDs, input.ActorUserID, reason); err != nil {
		return nil, fmt.Errorf("revoke invalid department manager grants: %w", err)
	}
	if err := s.replaceDepartmentGroupPool(ctx, tenantKey, deptID, groupIDs, input.ActorUserID); err != nil {
		return nil, fmt.Errorf("replace department group pool: %w", err)
	}
	for _, userID := range affectedUserIDs {
		if err := s.recalculateUserAllowedGroups(ctx, userID); err != nil {
			return nil, fmt.Errorf("recalculate allowed groups for user %d: %w", userID, err)
		}
		if s.authCacheInvalidator != nil {
			s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, userID)
		}
	}
	if err := s.appendAuditLog(ctx, input.ActorUserID, 0, deptID, FeishuGrantSourceDepartmentManager, "auto_revoke", reason); err != nil {
		return nil, fmt.Errorf("append audit log: %w", err)
	}
	return &FeishuDepartmentGroupPoolResult{
		TenantKey:        tenantKey,
		OpenDepartmentID: deptID,
		GroupIDs:         groupIDs,
		RevokedUserIDs:   affectedUserIDs,
	}, nil
}

func (s *FeishuOrgPermissionService) SetDepartmentManagerUserGroupGrants(ctx context.Context, input FeishuDepartmentManagerAssignmentInput) (*FeishuDepartmentManagerAssignmentResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	if input.ManagerUserID <= 0 {
		return nil, errors.New("manager_user_id is required")
	}
	if input.TargetUserID <= 0 {
		return nil, errors.New("target_user_id is required")
	}

	tenantKey, primaryDeptID, err := s.getUserPrimaryDepartment(ctx, input.TargetUserID)
	if err != nil {
		return nil, err
	}
	if primaryDeptID == "" {
		return nil, errors.New("target user has no primary feishu department")
	}

	canManage, err := s.canManagerManageDepartment(ctx, tenantKey, primaryDeptID, input.ManagerUserID)
	if err != nil {
		return nil, err
	}
	if !canManage {
		return nil, errors.New("manager cannot manage target user")
	}

	assignableGroupIDs, err := s.listDepartmentGroupPool(ctx, tenantKey, primaryDeptID)
	if err != nil {
		return nil, err
	}
	groupIDs := uniqueSortedInt64s(input.GroupIDs)
	if missing := firstMissingInt64(groupIDs, assignableGroupIDs); missing > 0 {
		return nil, fmt.Errorf("group %d is not in primary department group pool", missing)
	}

	grantResult, err := s.SetUserGroupGrants(ctx, FeishuSetUserGroupGrantsInput{
		ActorUserID:            input.ManagerUserID,
		TargetUserID:           input.TargetUserID,
		Source:                 FeishuGrantSourceDepartmentManager,
		SourceOpenDepartmentID: primaryDeptID,
		GroupIDs:               groupIDs,
		Reason:                 input.Reason,
	})
	if err != nil {
		return nil, err
	}
	return &FeishuDepartmentManagerAssignmentResult{
		FeishuUserGroupGrantResult: *grantResult,
		TenantKey:                  tenantKey,
		AssignableGroupIDs:         assignableGroupIDs,
	}, nil
}

func (s *FeishuOrgPermissionService) listUsersWithInvalidDepartmentManagerGrants(ctx context.Context, deptID string, allowedGroupIDs []int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT user_id
FROM feishu_user_group_grants
WHERE source = 'department_manager'
  AND source_open_department_id = $1
  AND revoked_at IS NULL
  AND NOT (group_id = ANY($2))
ORDER BY user_id`, deptID, pq.Array(allowedGroupIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedInt64s(ids), nil
}

func (s *FeishuOrgPermissionService) revokeInvalidDepartmentManagerGrants(ctx context.Context, deptID string, allowedGroupIDs []int64, actorUserID int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE feishu_user_group_grants
SET revoked_at = NOW(),
    revoked_by_user_id = NULLIF($3, 0),
    revoke_reason = $4,
    updated_at = NOW()
WHERE source = 'department_manager'
  AND source_open_department_id = $1
  AND revoked_at IS NULL
  AND NOT (group_id = ANY($2))`, deptID, pq.Array(allowedGroupIDs), actorUserID, reason)
	return err
}

func (s *FeishuOrgPermissionService) replaceDepartmentGroupPool(ctx context.Context, tenantKey, deptID string, groupIDs []int64, actorUserID int64) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM feishu_department_group_grants
WHERE tenant_key = $1
  AND open_department_id = $2
  AND NOT (group_id = ANY($3))`, tenantKey, deptID, pq.Array(groupIDs)); err != nil {
		return err
	}
	if len(groupIDs) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO feishu_department_group_grants (
    tenant_key,
    open_department_id,
    group_id,
    granted_by_user_id
)
SELECT $1,
       $2,
       UNNEST($3::bigint[]),
       NULLIF($4, 0)
ON CONFLICT (tenant_key, open_department_id, group_id)
DO UPDATE SET
    granted_by_user_id = EXCLUDED.granted_by_user_id,
    updated_at = NOW()`, tenantKey, deptID, pq.Array(groupIDs), actorUserID)
	return err
}

func (s *FeishuOrgPermissionService) SetUserGroupGrants(ctx context.Context, input FeishuSetUserGroupGrantsInput) (*FeishuUserGroupGrantResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	if input.TargetUserID <= 0 {
		return nil, errors.New("target_user_id is required")
	}
	source := normalizeFeishuGrantSource(input.Source)
	if source == "" {
		return nil, fmt.Errorf("unsupported grant source: %s", input.Source)
	}
	sourceDeptID := strings.TrimSpace(input.SourceOpenDepartmentID)
	if source == FeishuGrantSourceDepartmentManager && sourceDeptID == "" {
		return nil, errors.New("source_open_department_id is required for department manager grants")
	}
	if source != FeishuGrantSourceDepartmentManager {
		sourceDeptID = ""
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		reason = "manual"
	}
	groupIDs := uniqueSortedInt64s(input.GroupIDs)

	if err := s.ensureLegacyAllowedGroupsImported(ctx, input.TargetUserID, input.ActorUserID); err != nil {
		return nil, fmt.Errorf("import legacy allowed groups: %w", err)
	}
	if err := s.replaceActiveSourceGrants(ctx, input.TargetUserID, source, sourceDeptID, groupIDs, input.ActorUserID, reason); err != nil {
		return nil, fmt.Errorf("replace source grants: %w", err)
	}
	if err := s.recalculateUserAllowedGroups(ctx, input.TargetUserID); err != nil {
		return nil, fmt.Errorf("recalculate allowed groups: %w", err)
	}
	if err := s.appendAuditLog(ctx, input.ActorUserID, input.TargetUserID, sourceDeptID, source, "grant", reason); err != nil {
		return nil, fmt.Errorf("append audit log: %w", err)
	}
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(ctx, input.TargetUserID)
	}

	return &FeishuUserGroupGrantResult{
		UserID:                 input.TargetUserID,
		Source:                 source,
		SourceOpenDepartmentID: sourceDeptID,
		GroupIDs:               groupIDs,
	}, nil
}

func (s *FeishuOrgPermissionService) getUserPrimaryDepartment(ctx context.Context, userID int64) (tenantKey string, primaryDeptID string, err error) {
	err = scanFeishuSingleRow(ctx, s.db, `
SELECT tenant_key, primary_open_department_id
FROM feishu_org_users
WHERE user_id = $1
  AND status = 'active'
LIMIT 1`, []any{userID}, &tenantKey, &primaryDeptID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("target feishu org user is not active or not synced")
	}
	return strings.TrimSpace(tenantKey), strings.TrimSpace(primaryDeptID), err
}

func (s *FeishuOrgPermissionService) canManagerManageDepartment(ctx context.Context, tenantKey, targetDeptID string, managerUserID int64) (bool, error) {
	var count int64
	err := scanFeishuSingleRow(ctx, s.db, `
WITH RECURSIVE department_scope(open_department_id, parent_open_department_id) AS (
    SELECT open_department_id, parent_open_department_id
    FROM feishu_departments
    WHERE tenant_key = $1
      AND open_department_id = $2
      AND status = 'active'
    UNION ALL
    SELECT parent.open_department_id, parent.parent_open_department_id
    FROM feishu_departments parent
    JOIN department_scope child
      ON parent.tenant_key = $1
     AND parent.open_department_id = child.parent_open_department_id
    WHERE parent.status = 'active'
)
SELECT COUNT(*)
FROM feishu_department_managers manager
WHERE manager.tenant_key = $1
  AND manager.manager_user_id = $3
  AND manager.status = 'active'
  AND (
    manager.open_department_id = $2
    OR (
      manager.include_subdepartments = true
      AND manager.open_department_id IN (SELECT open_department_id FROM department_scope)
    )
  )`, []any{tenantKey, targetDeptID, managerUserID}, &count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *FeishuOrgPermissionService) listDepartmentGroupPool(ctx context.Context, tenantKey, deptID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT group_id
FROM feishu_department_group_grants
WHERE tenant_key = $1
  AND open_department_id = $2
ORDER BY group_id`, tenantKey, deptID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return uniqueSortedInt64s(ids), nil
}

func (s *FeishuOrgPermissionService) ensureLegacyAllowedGroupsImported(ctx context.Context, userID, actorUserID int64) error {
	var count int64
	if err := scanFeishuSingleRow(ctx, s.db, "SELECT COUNT(*) FROM feishu_user_group_grants WHERE user_id = $1", []any{userID}, &count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO feishu_user_group_grants (
    user_id,
    group_id,
    source,
    source_open_department_id,
    granted_by_user_id,
    reason
)
SELECT user_id,
       group_id,
       'super_admin_override',
       '',
       NULLIF($2, 0),
       'legacy_import'
FROM user_allowed_groups
WHERE user_id = $1
ON CONFLICT (user_id, group_id, source, source_open_department_id) WHERE revoked_at IS NULL
DO NOTHING`, userID, actorUserID)
	return err
}

func (s *FeishuOrgPermissionService) replaceActiveSourceGrants(ctx context.Context, userID int64, source, sourceDeptID string, groupIDs []int64, actorUserID int64, reason string) error {
	if _, err := s.db.ExecContext(ctx, `
UPDATE feishu_user_group_grants
SET revoked_at = NOW(),
    revoked_by_user_id = NULLIF($5, 0),
    revoke_reason = $6,
    updated_at = NOW()
WHERE user_id = $1
  AND source = $2
  AND source_open_department_id = $3
  AND revoked_at IS NULL
  AND NOT (group_id = ANY($4))`, userID, source, sourceDeptID, pq.Array(groupIDs), actorUserID, reason); err != nil {
		return err
	}
	if len(groupIDs) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO feishu_user_group_grants (
    user_id,
    group_id,
    source,
    source_open_department_id,
    granted_by_user_id,
    reason
)
SELECT $1,
       UNNEST($2::bigint[]),
       $3,
       $4,
       NULLIF($5, 0),
       $6
ON CONFLICT (user_id, group_id, source, source_open_department_id) WHERE revoked_at IS NULL
DO UPDATE SET
    granted_by_user_id = EXCLUDED.granted_by_user_id,
    reason = EXCLUDED.reason,
    updated_at = NOW()`, userID, pq.Array(groupIDs), source, sourceDeptID, actorUserID, reason)
	return err
}

func (s *FeishuOrgPermissionService) recalculateUserAllowedGroups(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM user_allowed_groups WHERE user_id = $1", userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO user_allowed_groups (user_id, group_id)
SELECT DISTINCT user_id, group_id
FROM feishu_user_group_grants
WHERE user_id = $1
  AND revoked_at IS NULL
ON CONFLICT (user_id, group_id) DO NOTHING`, userID)
	return err
}

func (s *FeishuOrgPermissionService) appendAuditLog(ctx context.Context, actorUserID, targetUserID int64, openDepartmentID, source, action, reason string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO feishu_org_permission_audit_logs (
    actor_user_id,
    target_user_id,
    open_department_id,
    source,
    action,
    reason
)
VALUES (NULLIF($1, 0), NULLIF($2, 0), $3, $4, $5, $6)`, actorUserID, targetUserID, openDepartmentID, source, action, reason)
	return err
}

type FeishuDeparturePolicy struct {
	Action           string
	ThresholdCount   int
	ThresholdPercent int
}

type FeishuDepartureDecision struct {
	AutoDisable    bool
	RequiresReview bool
	Reason         string
	UserIDs        []int64
}

func EvaluateFeishuDepartureDecision(previousActiveUsers int, departedUserIDs []int64, policy FeishuDeparturePolicy) FeishuDepartureDecision {
	userIDs := uniqueSortedInt64s(departedUserIDs)
	if len(userIDs) == 0 {
		return FeishuDepartureDecision{}
	}
	if strings.TrimSpace(policy.Action) != FeishuDepartedUserActionAutoDisable {
		return FeishuDepartureDecision{
			RequiresReview: true,
			Reason:         "FEISHU_DEPARTURE_REVIEW_REQUIRED",
			UserIDs:        userIDs,
		}
	}
	countThreshold := policy.ThresholdCount
	if countThreshold <= 0 {
		countThreshold = FeishuDefaultDisableThresholdCount
	}
	percentThreshold := policy.ThresholdPercent
	if percentThreshold <= 0 {
		percentThreshold = FeishuDefaultDisableThresholdPct
	}

	if len(userIDs) > countThreshold || departurePercent(previousActiveUsers, len(userIDs)) > percentThreshold {
		return FeishuDepartureDecision{
			RequiresReview: true,
			Reason:         "FEISHU_DEPARTURE_THRESHOLD_EXCEEDED",
			UserIDs:        userIDs,
		}
	}
	return FeishuDepartureDecision{
		AutoDisable: true,
		UserIDs:     userIDs,
	}
}

func normalizeFeishuGrantSource(source string) string {
	switch strings.TrimSpace(source) {
	case FeishuGrantSourceDepartmentManager:
		return FeishuGrantSourceDepartmentManager
	case FeishuGrantSourceSuperAdmin:
		return FeishuGrantSourceSuperAdmin
	default:
		return ""
	}
}

func scanFeishuSingleRow(ctx context.Context, q feishuOrgSQLExecutor, query string, args []any, dest ...any) (err error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()

	if !rows.Next() {
		if err = rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err = rows.Scan(dest...); err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return nil
}

func feishuOrgUsersListSQL(suffix string) string {
	return `
SELECT COALESCE(fu.user_id, 0) AS user_id,
       COALESCE(local_user.email, '') AS local_email,
       COALESCE(local_user.username, '') AS local_username,
       COALESCE(local_user.status, '') AS local_status,
       fu.tenant_key,
       fu.open_id,
       fu.union_id,
       fu.feishu_user_id,
       fu.name,
       fu.email,
       fu.employee_no,
       fu.status,
       fu.primary_open_department_id,
       COALESCE(primary_department.name, '') AS primary_department_name,
       COALESCE(primary_department.path, '') AS primary_department_path,
       fu.department_open_ids::text AS department_open_ids,
       COALESCE(department_grants.group_ids, '[]'::jsonb)::text AS department_manager_group_ids,
       COALESCE(override_grants.group_ids, '[]'::jsonb)::text AS super_admin_override_group_ids,
       COALESCE(effective_groups.group_ids, '[]'::jsonb)::text AS effective_group_ids,
       COALESCE(assignable_groups.groups, '[]'::jsonb)::text AS assignable_groups,
       fu.last_synced_at
FROM feishu_org_users fu
LEFT JOIN users local_user
  ON local_user.id = fu.user_id
LEFT JOIN feishu_departments primary_department
  ON primary_department.tenant_key = fu.tenant_key
 AND primary_department.open_department_id = fu.primary_open_department_id
LEFT JOIN (
    SELECT user_id, jsonb_agg(group_id ORDER BY group_id) AS group_ids
    FROM feishu_user_group_grants
    WHERE revoked_at IS NULL
      AND source = 'department_manager'
    GROUP BY user_id
) department_grants
  ON department_grants.user_id = fu.user_id
LEFT JOIN (
    SELECT user_id, jsonb_agg(group_id ORDER BY group_id) AS group_ids
    FROM feishu_user_group_grants
    WHERE revoked_at IS NULL
      AND source = 'super_admin_override'
    GROUP BY user_id
) override_grants
  ON override_grants.user_id = fu.user_id
LEFT JOIN (
    SELECT user_id, jsonb_agg(group_id ORDER BY group_id) AS group_ids
    FROM user_allowed_groups
    GROUP BY user_id
) effective_groups
  ON effective_groups.user_id = fu.user_id
LEFT JOIN LATERAL (
    SELECT jsonb_agg(
             jsonb_build_object(
               'id', groups.id,
               'name', groups.name,
               'platform', groups.platform,
               'subscription_type', groups.subscription_type
             )
             ORDER BY groups.sort_order, groups.id
           ) AS groups
    FROM feishu_department_group_grants grants
    JOIN groups
      ON groups.id = grants.group_id
     AND groups.deleted_at IS NULL
    WHERE grants.tenant_key = fu.tenant_key
      AND grants.open_department_id = fu.primary_open_department_id
) assignable_groups ON true
` + suffix
}

func scanFeishuOrgUserViews(rows *sql.Rows) ([]FeishuOrgUserView, error) {
	items := make([]FeishuOrgUserView, 0)
	for rows.Next() {
		var item FeishuOrgUserView
		var departmentRaw string
		var managerGrantRaw string
		var overrideGrantRaw string
		var effectiveRaw string
		var assignableRaw string
		var lastSyncedAt sql.NullTime
		if err := rows.Scan(
			&item.UserID,
			&item.LocalEmail,
			&item.LocalUsername,
			&item.LocalStatus,
			&item.TenantKey,
			&item.OpenID,
			&item.UnionID,
			&item.FeishuUserID,
			&item.Name,
			&item.Email,
			&item.EmployeeNo,
			&item.Status,
			&item.PrimaryOpenDepartmentID,
			&item.PrimaryDepartmentName,
			&item.PrimaryDepartmentPath,
			&departmentRaw,
			&managerGrantRaw,
			&overrideGrantRaw,
			&effectiveRaw,
			&assignableRaw,
			&lastSyncedAt,
		); err != nil {
			return nil, err
		}
		departmentIDs, err := decodeFeishuJSONStringSlice(departmentRaw)
		if err != nil {
			return nil, fmt.Errorf("decode feishu user departments: %w", err)
		}
		managerGroupIDs, err := decodeFeishuJSONInt64Slice(managerGrantRaw)
		if err != nil {
			return nil, fmt.Errorf("decode department manager grants: %w", err)
		}
		overrideGroupIDs, err := decodeFeishuJSONInt64Slice(overrideGrantRaw)
		if err != nil {
			return nil, fmt.Errorf("decode super admin override grants: %w", err)
		}
		effectiveGroupIDs, err := decodeFeishuJSONInt64Slice(effectiveRaw)
		if err != nil {
			return nil, fmt.Errorf("decode effective groups: %w", err)
		}
		assignableGroups, err := decodeFeishuJSONGroups(assignableRaw)
		if err != nil {
			return nil, fmt.Errorf("decode assignable groups: %w", err)
		}
		item.DepartmentOpenIDs = departmentIDs
		item.DepartmentManagerGroupIDs = managerGroupIDs
		item.SuperAdminOverrideGroupIDs = overrideGroupIDs
		item.EffectiveGroupIDs = effectiveGroupIDs
		item.AssignableGroups = assignableGroups
		item.LastSyncedAt = sqlNullTimePtr(lastSyncedAt)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func normalizeFeishuOrgListInput(input FeishuOrgListInput) (tenantKey string, limit int, offset int) {
	tenantKey = strings.TrimSpace(input.TenantKey)
	limit = input.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset = input.Offset
	if offset < 0 {
		offset = 0
	}
	return tenantKey, limit, offset
}

func decodeFeishuJSONStringSlice(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out, nil
}

func decodeFeishuJSONInt64Slice(raw string) ([]int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var values []int64
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	return uniqueSortedInt64s(values), nil
}

func decodeFeishuJSONGroups(raw string) ([]FeishuOrgGroupBrief, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var groups []FeishuOrgGroupBrief
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		return nil, err
	}
	out := make([]FeishuOrgGroupBrief, 0, len(groups))
	for _, group := range groups {
		if group.ID > 0 {
			out = append(out, group)
		}
	}
	return out, nil
}

func sqlNullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time
	return &t
}

func departurePercent(total int, departed int) int {
	if total <= 0 || departed <= 0 {
		return 0
	}
	return (departed * 100) / total
}

func firstString(values []string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func containsString(values []string, target string) bool {
	target = strings.TrimSpace(target)
	if target == "" {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func firstMissingInt64(required []int64, allowed []int64) int64 {
	if len(required) == 0 {
		return 0
	}
	allowedSet := make(map[int64]struct{}, len(allowed))
	for _, id := range allowed {
		if id > 0 {
			allowedSet[id] = struct{}{}
		}
	}
	for _, id := range required {
		if id <= 0 {
			continue
		}
		if _, ok := allowedSet[id]; !ok {
			return id
		}
	}
	return 0
}

func uniqueSortedInt64s(values []int64) []int64 {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
