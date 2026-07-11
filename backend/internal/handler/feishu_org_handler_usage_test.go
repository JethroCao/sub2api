package handler

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFeishuOrgHandlerManagerUsageStatsScopesToManagedLocalUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("WITH RECURSIVE manager_roots")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).
			AddRow(int64(42)).
			AddRow(int64(99)))

	repo := &userUsageRepoCapture{}
	usageSvc := service.NewUsageService(repo, nil, nil, nil)
	orgSvc := service.NewFeishuOrgPermissionService(db, nil)
	handler := NewFeishuOrgHandler(orgSvc, nil, usageSvc)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		c.Next()
	})
	router.GET("/org-manager/usage/stats", handler.ManagerUsageStats)

	req := httptest.NewRequest(http.MethodGet, "/org-manager/usage/stats?start_date=2026-03-01&end_date=2026-03-02", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []int64{42, 99}, repo.statsFilters.UserIDs)
	require.Equal(t, int64(0), repo.statsFilters.UserID)
	require.NotNil(t, repo.statsFilters.StartTime)
	require.NotNil(t, repo.statsFilters.EndTime)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgHandlerManagerUsageStatsUsesEmptyScopeWhenNoBoundUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("WITH RECURSIVE manager_roots")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}))

	repo := &userUsageRepoCapture{}
	usageSvc := service.NewUsageService(repo, nil, nil, nil)
	orgSvc := service.NewFeishuOrgPermissionService(db, nil)
	handler := NewFeishuOrgHandler(orgSvc, nil, usageSvc)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		c.Next()
	})
	router.GET("/org-manager/usage/stats", handler.ManagerUsageStats)

	req := httptest.NewRequest(http.MethodGet, "/org-manager/usage/stats?start_date=2026-03-01&end_date=2026-03-02", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, repo.statsFilters.UserIDs)
	require.Empty(t, repo.statsFilters.UserIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgHandlerListManagerUsageCanFilterManagedUser(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("WITH RECURSIVE manager_roots")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).
			AddRow(int64(42)).
			AddRow(int64(99)))

	repo := &userUsageRepoCapture{}
	usageSvc := service.NewUsageService(repo, nil, nil, nil)
	orgSvc := service.NewFeishuOrgPermissionService(db, nil)
	handler := NewFeishuOrgHandler(orgSvc, nil, usageSvc)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		c.Next()
	})
	router.GET("/org-manager/usage", handler.ListManagerUsage)

	req := httptest.NewRequest(http.MethodGet, "/org-manager/usage?user_id=99&page=1&page_size=20", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []int64{99}, repo.listFilters.UserIDs)
	require.Equal(t, int64(0), repo.listFilters.UserID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFeishuOrgHandlerListManagerUsageRejectsUnmanagedUserFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(regexp.QuoteMeta("WITH RECURSIVE manager_roots")).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).
			AddRow(int64(42)).
			AddRow(int64(99)))

	repo := &userUsageRepoCapture{}
	usageSvc := service.NewUsageService(repo, nil, nil, nil)
	orgSvc := service.NewFeishuOrgPermissionService(db, nil)
	handler := NewFeishuOrgHandler(orgSvc, nil, usageSvc)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 7})
		c.Next()
	})
	router.GET("/org-manager/usage", handler.ListManagerUsage)

	req := httptest.NewRequest(http.MethodGet, "/org-manager/usage?user_id=100&page=1&page_size=20", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Empty(t, repo.listFilters.UserIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}
