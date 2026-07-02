package service

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
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

func (s *feishuOrgPermissionInvalidatorStub) InvalidateAuthCacheByKey(context.Context, string) {}

func (s *feishuOrgPermissionInvalidatorStub) InvalidateAuthCacheByGroupID(context.Context, int64) {}

func (s *feishuOrgPermissionInvalidatorStub) InvalidateAuthCacheByUserID(_ context.Context, userID int64) {
	s.userIDs = append(s.userIDs, userID)
}
