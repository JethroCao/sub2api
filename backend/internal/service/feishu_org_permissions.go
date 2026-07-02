package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

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
