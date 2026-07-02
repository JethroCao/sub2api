package service

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestFeishuOrgPermissionSnapshotSeparatesDepartmentPoolAssignmentsAndPersonalOverride(t *testing.T) {
	snapshot := FeishuOrgPermissionSnapshot{
		ManagerDepartmentIDs:        []string{"dept-a"},
		EmployeePrimaryDepartmentID: "dept-a",
		EmployeeDepartmentIDs:       []string{"dept-a", "dept-side"},
		DepartmentAssignableGroupIDs: map[string][]int64{
			"dept-a":    {11, 12},
			"dept-side": {99},
		},
		DepartmentManagerGroupGrants: []int64{11},
		UserOverrideGroupGrants:      []int64{77},
	}

	require.Equal(t, []int64{11, 12}, snapshot.ManagerAssignableGroupIDs())
	require.True(t, snapshot.CanManagerAssignGroup(11))
	require.False(t, snapshot.CanManagerAssignGroup(77), "个人 override 不会扩大部门领导的可分配池")
	require.Equal(t, []int64{11, 77}, snapshot.EffectiveEmployeeGroupIDs(), "部门池只是可分配范围，不会自动全量发给员工")
}

func TestFeishuOrgPermissionSnapshotUsesPrimaryDepartmentForMultiDepartmentEmployee(t *testing.T) {
	snapshot := FeishuOrgPermissionSnapshot{
		ManagerDepartmentIDs:        []string{"dept-side"},
		EmployeePrimaryDepartmentID: "dept-a",
		EmployeeDepartmentIDs:       []string{"dept-a", "dept-side"},
		DepartmentAssignableGroupIDs: map[string][]int64{
			"dept-a":    {11},
			"dept-side": {99},
		},
	}

	require.Empty(t, snapshot.ManagerAssignableGroupIDs())
	require.False(t, snapshot.CanManagerAssignGroup(99))
}

func TestFeishuOrgPermissionServiceSetDepartmentManagerGrantImportsLegacyAndRecalculates(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM feishu_user_group_grants WHERE user_id = $1")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec("INSERT INTO feishu_user_group_grants").
		WithArgs(int64(42), int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE feishu_user_group_grants").
		WithArgs(int64(42), FeishuGrantSourceDepartmentManager, "dept-a", sqlmock.AnyArg(), int64(7), "manual").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO feishu_user_group_grants").
		WithArgs(int64(42), sqlmock.AnyArg(), FeishuGrantSourceDepartmentManager, "dept-a", int64(7), "manual").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM user_allowed_groups WHERE user_id = $1")).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO user_allowed_groups").
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO feishu_org_permission_audit_logs").
		WithArgs(int64(7), int64(42), "dept-a", FeishuGrantSourceDepartmentManager, "grant", "manual").
		WillReturnResult(sqlmock.NewResult(0, 1))

	invalidator := &feishuOrgPermissionInvalidatorStub{}
	svc := NewFeishuOrgPermissionService(db, invalidator)

	result, err := svc.SetUserGroupGrants(context.Background(), FeishuSetUserGroupGrantsInput{
		ActorUserID:            7,
		TargetUserID:           42,
		Source:                 FeishuGrantSourceDepartmentManager,
		SourceOpenDepartmentID: "dept-a",
		GroupIDs:               []int64{11},
		Reason:                 "manual",
	})

	require.NoError(t, err)
	require.Equal(t, []int64{11}, result.GroupIDs)
	require.Equal(t, []int64{42}, invalidator.userIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceRejectsDepartmentGrantWithoutDepartment(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	svc := NewFeishuOrgPermissionService(db, nil)
	_, err = svc.SetUserGroupGrants(context.Background(), FeishuSetUserGroupGrantsInput{
		ActorUserID:  7,
		TargetUserID: 42,
		Source:       FeishuGrantSourceDepartmentManager,
		GroupIDs:     []int64{11},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "source_open_department_id is required")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceSetDepartmentManagerUserGroupGrants(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT tenant_key, primary_open_department_id FROM feishu_org_users").
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_key", "primary_open_department_id"}).AddRow("tenant-a", "dept-a"))
	mock.ExpectQuery("WITH RECURSIVE department_scope").
		WithArgs("tenant-a", "dept-a", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT group_id FROM feishu_department_group_grants").
		WithArgs("tenant-a", "dept-a").
		WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(11).AddRow(12))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM feishu_user_group_grants WHERE user_id = $1")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec("UPDATE feishu_user_group_grants").
		WithArgs(int64(42), FeishuGrantSourceDepartmentManager, "dept-a", sqlmock.AnyArg(), int64(7), "manual").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("INSERT INTO feishu_user_group_grants").
		WithArgs(int64(42), sqlmock.AnyArg(), FeishuGrantSourceDepartmentManager, "dept-a", int64(7), "manual").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM user_allowed_groups WHERE user_id = $1")).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO user_allowed_groups").
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO feishu_org_permission_audit_logs").
		WithArgs(int64(7), int64(42), "dept-a", FeishuGrantSourceDepartmentManager, "grant", "manual").
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewFeishuOrgPermissionService(db, nil)
	result, err := svc.SetDepartmentManagerUserGroupGrants(context.Background(), FeishuDepartmentManagerAssignmentInput{
		ManagerUserID: 7,
		TargetUserID:  42,
		GroupIDs:      []int64{12},
	})

	require.NoError(t, err)
	require.Equal(t, "dept-a", result.SourceOpenDepartmentID)
	require.Equal(t, []int64{12}, result.GroupIDs)
	require.Equal(t, []int64{11, 12}, result.AssignableGroupIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceRejectsGroupOutsidePrimaryDepartmentPool(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT tenant_key, primary_open_department_id FROM feishu_org_users").
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_key", "primary_open_department_id"}).AddRow("tenant-a", "dept-a"))
	mock.ExpectQuery("WITH RECURSIVE department_scope").
		WithArgs("tenant-a", "dept-a", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery("SELECT group_id FROM feishu_department_group_grants").
		WithArgs("tenant-a", "dept-a").
		WillReturnRows(sqlmock.NewRows([]string{"group_id"}).AddRow(11))

	svc := NewFeishuOrgPermissionService(db, nil)
	_, err = svc.SetDepartmentManagerUserGroupGrants(context.Background(), FeishuDepartmentManagerAssignmentInput{
		ManagerUserID: 7,
		TargetUserID:  42,
		GroupIDs:      []int64{99},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "not in primary department group pool")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceRejectsManagerOutsideDepartmentScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT tenant_key, primary_open_department_id FROM feishu_org_users").
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_key", "primary_open_department_id"}).AddRow("tenant-a", "dept-a"))
	mock.ExpectQuery("WITH RECURSIVE department_scope").
		WithArgs("tenant-a", "dept-a", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	svc := NewFeishuOrgPermissionService(db, nil)
	_, err = svc.SetDepartmentManagerUserGroupGrants(context.Background(), FeishuDepartmentManagerAssignmentInput{
		ManagerUserID: 7,
		TargetUserID:  42,
		GroupIDs:      []int64{11},
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "manager cannot manage target user")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceSetDepartmentGroupPoolRevokesInvalidManagerGrants(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("SELECT DISTINCT user_id FROM feishu_user_group_grants").
		WithArgs("dept-a", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(42).AddRow(43))
	mock.ExpectExec("UPDATE feishu_user_group_grants").
		WithArgs("dept-a", sqlmock.AnyArg(), int64(1), "pool_updated").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("DELETE FROM feishu_department_group_grants").
		WithArgs("tenant-a", "dept-a", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO feishu_department_group_grants").
		WithArgs("tenant-a", "dept-a", sqlmock.AnyArg(), int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM user_allowed_groups WHERE user_id = $1")).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO user_allowed_groups").
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM user_allowed_groups WHERE user_id = $1")).
		WithArgs(int64(43)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO user_allowed_groups").
		WithArgs(int64(43)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO feishu_org_permission_audit_logs").
		WithArgs(int64(1), int64(0), "dept-a", FeishuGrantSourceDepartmentManager, "auto_revoke", "pool_updated").
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewFeishuOrgPermissionService(db, &feishuOrgPermissionInvalidatorStub{})
	result, err := svc.SetDepartmentGroupPool(context.Background(), FeishuSetDepartmentGroupPoolInput{
		ActorUserID:      1,
		TenantKey:        "tenant-a",
		OpenDepartmentID: "dept-a",
		GroupIDs:         []int64{11},
		Reason:           "pool_updated",
	})

	require.NoError(t, err)
	require.Equal(t, []int64{11}, result.GroupIDs)
	require.Equal(t, []int64{42, 43}, result.RevokedUserIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceListDepartmentsIncludesAssignableGroups(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	syncedAt := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT d.tenant_key").
		WithArgs("tenant-a", 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"tenant_key",
			"open_department_id",
			"parent_open_department_id",
			"name",
			"path",
			"status",
			"leader_open_ids",
			"last_synced_at",
			"employee_count",
			"manager_count",
			"assignable_groups",
		}).AddRow(
			"tenant-a",
			"dept-a",
			"",
			"研发部",
			"/研发部",
			"active",
			`["ou_manager"]`,
			syncedAt,
			int64(3),
			int64(1),
			`[{"id":11,"name":"Claude Code","platform":"anthropic","subscription_type":"pro"}]`,
		))

	svc := NewFeishuOrgPermissionService(db, nil)
	result, err := svc.ListDepartments(context.Background(), FeishuOrgListInput{
		TenantKey: "tenant-a",
		Limit:     50,
	})

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	dept := result.Items[0]
	require.Equal(t, "dept-a", dept.OpenDepartmentID)
	require.Equal(t, []string{"ou_manager"}, dept.LeaderOpenIDs)
	require.Equal(t, int64(3), dept.EmployeeCount)
	require.Equal(t, int64(1), dept.ManagerCount)
	require.Equal(t, int64(11), dept.AssignableGroups[0].ID)
	require.Equal(t, "Claude Code", dept.AssignableGroups[0].Name)
	require.NotNil(t, dept.LastSyncedAt)
	require.Equal(t, syncedAt, *dept.LastSyncedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceListManagerUsersIncludesAssignableGroups(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery("WITH RECURSIVE manager_roots").
		WithArgs(int64(7), 50, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"user_id",
			"local_email",
			"local_username",
			"local_status",
			"tenant_key",
			"open_id",
			"union_id",
			"feishu_user_id",
			"name",
			"email",
			"employee_no",
			"status",
			"primary_open_department_id",
			"primary_department_name",
			"primary_department_path",
			"department_open_ids",
			"department_manager_group_ids",
			"super_admin_override_group_ids",
			"effective_group_ids",
			"assignable_groups",
			"last_synced_at",
		}).AddRow(
			int64(42),
			"a@example.com",
			"员工A",
			"active",
			"tenant-a",
			"ou_a",
			"on_a",
			"u_a",
			"员工A",
			"a@example.com",
			"E001",
			"active",
			"dept-a",
			"研发部",
			"/研发部",
			`["dept-a","dept-side"]`,
			`[11]`,
			`[77]`,
			`[11,77]`,
			`[{"id":11,"name":"Claude Code","platform":"anthropic","subscription_type":"pro"}]`,
			nil,
		))

	svc := NewFeishuOrgPermissionService(db, nil)
	result, err := svc.ListManagerUsers(context.Background(), 7, FeishuOrgListInput{Limit: 50})

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	user := result.Items[0]
	require.Equal(t, int64(42), user.UserID)
	require.Equal(t, "dept-a", user.PrimaryOpenDepartmentID)
	require.Equal(t, []string{"dept-a", "dept-side"}, user.DepartmentOpenIDs)
	require.Equal(t, []int64{11}, user.DepartmentManagerGroupIDs)
	require.Equal(t, []int64{77}, user.SuperAdminOverrideGroupIDs)
	require.Equal(t, []int64{11, 77}, user.EffectiveGroupIDs)
	require.Equal(t, int64(11), user.AssignableGroups[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceListSyncRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	startedAt := time.Date(2026, 7, 3, 13, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 7, 3, 13, 1, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT id, status, started_at").
		WithArgs(20, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"status",
			"started_at",
			"finished_at",
			"departments_synced",
			"users_synced",
			"managers_synced",
			"users_to_create",
			"users_to_disable",
			"bindings_missing",
			"review_required",
			"error_message",
			"triggered_by_user_id",
		}).AddRow(
			int64(9),
			"success",
			startedAt,
			finishedAt,
			3,
			12,
			2,
			1,
			0,
			4,
			false,
			"",
			int64(1),
		))

	svc := NewFeishuOrgPermissionService(db, nil)
	result, err := svc.ListSyncRuns(context.Background(), FeishuOrgListInput{Limit: 20})

	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	run := result.Items[0]
	require.Equal(t, int64(9), run.ID)
	require.Equal(t, "success", run.Status)
	require.Equal(t, 12, run.UsersSynced)
	require.Equal(t, 4, run.BindingsMissing)
	require.NotNil(t, run.FinishedAt)
	require.Equal(t, finishedAt, *run.FinishedAt)
	require.Equal(t, int64(1), run.TriggeredByUserID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceRunManualReconcileAutoDisablesDepartedUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	startedAt := time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 7, 3, 14, 1, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM feishu_org_users").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(100))
	mock.ExpectQuery("SELECT DISTINCT org_user.user_id").
		WithArgs(StatusActive, RoleAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(int64(42)).AddRow(int64(43)))
	mock.ExpectExec("UPDATE users").
		WithArgs(StatusDisabled, sqlmock.AnyArg(), RoleAdmin).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec("INSERT INTO feishu_org_permission_audit_logs").
		WithArgs(int64(7), int64(42), "", "", "auto_disable_user", "feishu_departure_auto_disable").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO feishu_org_permission_audit_logs").
		WithArgs(int64(7), int64(43), "", "", "auto_disable_user", "feishu_departure_auto_disable").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO feishu_org_sync_runs").
		WithArgs("success", 2, false, "", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"status",
			"started_at",
			"finished_at",
			"departments_synced",
			"users_synced",
			"managers_synced",
			"users_to_create",
			"users_to_disable",
			"bindings_missing",
			"review_required",
			"error_message",
			"triggered_by_user_id",
		}).AddRow(int64(10), "success", startedAt, finishedAt, 0, 0, 0, 0, 2, 0, false, "", int64(7)))

	invalidator := &feishuOrgPermissionInvalidatorStub{}
	svc := NewFeishuOrgPermissionService(db, invalidator)
	result, err := svc.RunManualReconcile(context.Background(), 7, FeishuDeparturePolicy{
		Action:           FeishuDepartedUserActionAutoDisable,
		ThresholdCount:   10,
		ThresholdPercent: 20,
	})

	require.NoError(t, err)
	require.True(t, result.Decision.AutoDisable)
	require.Equal(t, []int64{42, 43}, result.Decision.UserIDs)
	require.Equal(t, int64(10), result.SyncRun.ID)
	require.Equal(t, []int64{42, 43}, invalidator.userIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgPermissionServiceRunManualReconcileRequiresReviewWhenThresholdExceeded(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	startedAt := time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 7, 3, 14, 1, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM feishu_org_users").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(10))
	mock.ExpectQuery("SELECT DISTINCT org_user.user_id").
		WithArgs(StatusActive, RoleAdmin).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(int64(42)).AddRow(int64(43)).AddRow(int64(44)))
	mock.ExpectExec("INSERT INTO feishu_org_permission_audit_logs").
		WithArgs(int64(7), int64(0), "", "", "sync_blocked_for_review", "FEISHU_DEPARTURE_THRESHOLD_EXCEEDED").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("INSERT INTO feishu_org_sync_runs").
		WithArgs("partial_failed", 3, true, "FEISHU_DEPARTURE_THRESHOLD_EXCEEDED", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"status",
			"started_at",
			"finished_at",
			"departments_synced",
			"users_synced",
			"managers_synced",
			"users_to_create",
			"users_to_disable",
			"bindings_missing",
			"review_required",
			"error_message",
			"triggered_by_user_id",
		}).AddRow(int64(11), "partial_failed", startedAt, finishedAt, 0, 0, 0, 0, 3, 0, true, "FEISHU_DEPARTURE_THRESHOLD_EXCEEDED", int64(7)))

	svc := NewFeishuOrgPermissionService(db, nil)
	result, err := svc.RunManualReconcile(context.Background(), 7, FeishuDeparturePolicy{
		Action:           FeishuDepartedUserActionAutoDisable,
		ThresholdCount:   10,
		ThresholdPercent: 20,
	})

	require.NoError(t, err)
	require.True(t, result.Decision.RequiresReview)
	require.Equal(t, "FEISHU_DEPARTURE_THRESHOLD_EXCEEDED", result.Decision.Reason)
	require.Equal(t, "partial_failed", result.SyncRun.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgDirectorySnapshotNormalizesRawFields(t *testing.T) {
	snapshot := normalizeFeishuDirectorySnapshot(&FeishuOrgDirectorySnapshot{
		TenantKey: "tenant-a",
		Departments: []FeishuOrgDirectoryDepartment{
			parseFeishuDirectoryDepartment(map[string]any{
				"department_id":        "od-a",
				"parent_department_id": "0",
				"name":                 "Engineering",
				"leaders": []any{
					map[string]any{"open_id": "ou-manager"},
				},
			}),
		},
		Users: []FeishuOrgDirectoryUser{
			parseFeishuDirectoryUser(map[string]any{
				"open_id":          "ou-a",
				"union_id":         "on-a",
				"user_id":          "ou-a",
				"name":             "Alice",
				"enterprise_email": "alice@example.com",
				"employee_no":      "E001",
				"department_ids":   []any{"od-b", "od-a"},
				"status": map[string]any{
					"is_frozen": true,
				},
			}),
		},
	})

	require.Len(t, snapshot.Departments, 1)
	require.Equal(t, "tenant-a", snapshot.Departments[0].TenantKey)
	require.Equal(t, "od-a", snapshot.Departments[0].OpenDepartmentID)
	require.Equal(t, []string{"ou-manager"}, snapshot.Departments[0].LeaderOpenIDs)
	require.Equal(t, "Engineering", snapshot.Departments[0].Path)
	require.Len(t, snapshot.Users, 1)
	require.Equal(t, "tenant-a", snapshot.Users[0].TenantKey)
	require.Equal(t, "disabled", snapshot.Users[0].Status)
	require.Equal(t, "od-a", snapshot.Users[0].PrimaryOpenDepartmentID)
	require.Equal(t, []string{"od-a", "od-b"}, snapshot.Users[0].DepartmentOpenIDs)
}

func TestFeishuOrgDirectoryHTTPClientRequiresTenantKey(t *testing.T) {
	_, err := NewFeishuOrgDirectoryHTTPClient(config.FeishuConnectConfig{
		AppID:     "cli_test",
		AppSecret: "secret",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "tenant key")
}

func TestFeishuOrgPermissionServiceRunFeishuSyncRejectsEmptyUserSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	startedAt := time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 7, 3, 14, 1, 0, 0, time.UTC)
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM feishu_org_users").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))
	mock.ExpectQuery("INSERT INTO feishu_org_sync_runs").
		WithArgs("failed", 0, 0, 0, 0, 0, false, "feishu org sync returned no users", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id",
			"status",
			"started_at",
			"finished_at",
			"departments_synced",
			"users_synced",
			"managers_synced",
			"users_to_create",
			"users_to_disable",
			"bindings_missing",
			"review_required",
			"error_message",
			"triggered_by_user_id",
		}).AddRow(int64(12), "failed", startedAt, finishedAt, 0, 0, 0, 0, 0, 0, false, "feishu org sync returned no users", int64(7)))

	svc := NewFeishuOrgPermissionService(db, nil)
	_, err = svc.RunFeishuOrgSyncWithClient(context.Background(), 7, feishuOrgDirectoryClientStub{
		snapshot: &FeishuOrgDirectorySnapshot{TenantKey: "tenant-a"},
	}, FeishuDeparturePolicy{Action: FeishuDepartedUserActionAutoDisable})

	require.Error(t, err)
	require.Contains(t, err.Error(), "no users")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluateFeishuDepartureDecisionAutoDisablesWithinThreshold(t *testing.T) {
	got := EvaluateFeishuDepartureDecision(100, []int64{1, 2}, FeishuDeparturePolicy{
		Action:           FeishuDepartedUserActionAutoDisable,
		ThresholdCount:   10,
		ThresholdPercent: 20,
	})

	require.True(t, got.AutoDisable)
	require.False(t, got.RequiresReview)
	require.Equal(t, []int64{1, 2}, got.UserIDs)
}

func TestEvaluateFeishuDepartureDecisionRequiresReviewWhenThresholdExceeded(t *testing.T) {
	got := EvaluateFeishuDepartureDecision(10, []int64{1, 2, 3}, FeishuDeparturePolicy{
		Action:           FeishuDepartedUserActionAutoDisable,
		ThresholdCount:   10,
		ThresholdPercent: 20,
	})

	require.False(t, got.AutoDisable)
	require.True(t, got.RequiresReview)
	require.Equal(t, "FEISHU_DEPARTURE_THRESHOLD_EXCEEDED", got.Reason)
}

type feishuOrgPermissionInvalidatorStub struct {
	userIDs []int64
}

type feishuOrgDirectoryClientStub struct {
	snapshot *FeishuOrgDirectorySnapshot
	err      error
}

func (s feishuOrgDirectoryClientStub) FetchSnapshot(context.Context) (*FeishuOrgDirectorySnapshot, error) {
	return s.snapshot, s.err
}

func (s *feishuOrgPermissionInvalidatorStub) InvalidateAuthCacheByKey(context.Context, string) {}

func (s *feishuOrgPermissionInvalidatorStub) InvalidateAuthCacheByGroupID(context.Context, int64) {}

func (s *feishuOrgPermissionInvalidatorStub) InvalidateAuthCacheByUserID(_ context.Context, userID int64) {
	s.userIDs = append(s.userIDs, userID)
}
