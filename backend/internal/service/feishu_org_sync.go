package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/lib/pq"
)

const (
	feishuOpenAPIBaseURL          = "https://open.feishu.cn"
	feishuRootDepartmentID        = "0"
	feishuOrgSyncHTTPTimeout      = 20 * time.Second
	feishuOrgSyncPageSize         = 50
	feishuOrgSyncResponseBodySize = 4 << 20
)

type FeishuOrgDirectoryClient interface {
	FetchSnapshot(ctx context.Context) (*FeishuOrgDirectorySnapshot, error)
}

type FeishuOrgDirectorySnapshot struct {
	TenantKey   string
	Departments []FeishuOrgDirectoryDepartment
	Users       []FeishuOrgDirectoryUser
	FetchedAt   time.Time
}

type FeishuOrgDirectoryDepartment struct {
	TenantKey              string
	OpenDepartmentID       string
	ParentOpenDepartmentID string
	Name                   string
	Path                   string
	Status                 string
	LeaderOpenIDs          []string
	Raw                    map[string]any
}

type FeishuOrgDirectoryUser struct {
	TenantKey               string
	OpenID                  string
	UnionID                 string
	FeishuUserID            string
	Name                    string
	Email                   string
	EmployeeNo              string
	Status                  string
	PrimaryOpenDepartmentID string
	ManagerOpenID           string
	DepartmentOpenIDs       []string
	Raw                     map[string]any
}

type feishuOrgSyncImportCounts struct {
	DepartmentsSynced int
	UsersSynced       int
	ManagersSynced    int
	BindingsMissing   int
}

type FeishuOrgDirectoryHTTPClient struct {
	appID     string
	appSecret string
	tenantKey string
	baseURL   string
	http      *http.Client
}

func NewFeishuOrgDirectoryHTTPClient(cfg config.FeishuConnectConfig) (*FeishuOrgDirectoryHTTPClient, error) {
	tenantKey := strings.TrimSpace(cfg.AllowedTenantKey)
	if tenantKey == "" {
		return nil, infraerrors.BadRequest(
			"FEISHU_TENANT_KEY_REQUIRED",
			"feishu org sync requires allowed tenant key",
		)
	}
	appID := strings.TrimSpace(cfg.AppID)
	if appID == "" {
		return nil, infraerrors.BadRequest("FEISHU_APP_ID_REQUIRED", "feishu app id is required")
	}
	appSecret := strings.TrimSpace(cfg.AppSecret)
	if appSecret == "" {
		return nil, infraerrors.BadRequest("FEISHU_APP_SECRET_REQUIRED", "feishu app secret is required")
	}
	baseURL := feishuOpenAPIBaseURL
	if parsed, err := url.Parse(strings.TrimSpace(cfg.UserInfoURL)); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		baseURL = parsed.Scheme + "://" + parsed.Host
	}
	return &FeishuOrgDirectoryHTTPClient{
		appID:     appID,
		appSecret: appSecret,
		tenantKey: tenantKey,
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      &http.Client{Timeout: feishuOrgSyncHTTPTimeout},
	}, nil
}

func (c *FeishuOrgDirectoryHTTPClient) FetchSnapshot(ctx context.Context) (*FeishuOrgDirectorySnapshot, error) {
	if c == nil {
		return nil, errors.New("feishu org directory client is not configured")
	}
	token, err := c.fetchTenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	departments, err := c.fetchDepartments(ctx, token)
	if err != nil {
		return nil, err
	}
	users, err := c.fetchUsers(ctx, token, departments)
	if err != nil {
		return nil, err
	}
	return normalizeFeishuDirectorySnapshot(&FeishuOrgDirectorySnapshot{
		TenantKey:   c.tenantKey,
		Departments: departments,
		Users:       users,
		FetchedAt:   time.Now().UTC(),
	}), nil
}

func (c *FeishuOrgDirectoryHTTPClient) fetchTenantAccessToken(ctx context.Context) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"app_id":     c.appID,
		"app_secret": c.appSecret,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/open-apis/auth/v3/tenant_access_token/internal", nil, "", bytes.NewReader(payload), &out); err != nil {
		return "", err
	}
	if out.Code != 0 {
		return "", fmt.Errorf("feishu tenant access token error code=%d msg=%s", out.Code, out.Msg)
	}
	if strings.TrimSpace(out.TenantAccessToken) == "" {
		return "", errors.New("feishu tenant access token response missing token")
	}
	return out.TenantAccessToken, nil
}

func (c *FeishuOrgDirectoryHTTPClient) fetchDepartments(ctx context.Context, token string) ([]FeishuOrgDirectoryDepartment, error) {
	var departments []FeishuOrgDirectoryDepartment
	pageToken := ""
	for {
		query := url.Values{}
		query.Set("department_id_type", "open_department_id")
		query.Set("user_id_type", "open_id")
		query.Set("parent_department_id", feishuRootDepartmentID)
		query.Set("page_size", fmt.Sprintf("%d", feishuOrgSyncPageSize))
		query.Set("fetch_child", "true")
		if pageToken != "" {
			query.Set("page_token", pageToken)
		}
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items     []map[string]any `json:"items"`
				HasMore   bool             `json:"has_more"`
				PageToken string           `json:"page_token"`
			} `json:"data"`
		}
		if err := c.doJSON(ctx, http.MethodGet, "/open-apis/contact/v3/departments", query, token, nil, &out); err != nil {
			return nil, err
		}
		if out.Code != 0 {
			return nil, fmt.Errorf("feishu departments error code=%d msg=%s", out.Code, out.Msg)
		}
		for _, raw := range out.Data.Items {
			dept := parseFeishuDirectoryDepartment(raw)
			dept.TenantKey = c.tenantKey
			if dept.OpenDepartmentID == "" || dept.OpenDepartmentID == feishuRootDepartmentID {
				continue
			}
			departments = append(departments, dept)
		}
		if !out.Data.HasMore || strings.TrimSpace(out.Data.PageToken) == "" {
			break
		}
		pageToken = strings.TrimSpace(out.Data.PageToken)
	}
	return departments, nil
}

func (c *FeishuOrgDirectoryHTTPClient) fetchUsers(ctx context.Context, token string, departments []FeishuOrgDirectoryDepartment) ([]FeishuOrgDirectoryUser, error) {
	departmentIDs := []string{feishuRootDepartmentID}
	for _, department := range departments {
		departmentIDs = append(departmentIDs, department.OpenDepartmentID)
	}
	seenDepartments := uniqueSortedStrings(departmentIDs)
	seenUsers := make(map[string]FeishuOrgDirectoryUser)
	for _, departmentID := range seenDepartments {
		pageToken := ""
		for {
			query := url.Values{}
			query.Set("department_id", departmentID)
			query.Set("department_id_type", "open_department_id")
			query.Set("user_id_type", "open_id")
			query.Set("page_size", fmt.Sprintf("%d", feishuOrgSyncPageSize))
			if pageToken != "" {
				query.Set("page_token", pageToken)
			}
			var out struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
				Data struct {
					Items     []map[string]any `json:"items"`
					HasMore   bool             `json:"has_more"`
					PageToken string           `json:"page_token"`
				} `json:"data"`
			}
			if err := c.doJSON(ctx, http.MethodGet, "/open-apis/contact/v3/users/find_by_department", query, token, nil, &out); err != nil {
				return nil, err
			}
			if out.Code != 0 {
				return nil, fmt.Errorf("feishu users by department %s error code=%d msg=%s", departmentID, out.Code, out.Msg)
			}
			for _, raw := range out.Data.Items {
				user := parseFeishuDirectoryUser(raw)
				user.TenantKey = c.tenantKey
				if user.OpenID == "" {
					continue
				}
				if departmentID != feishuRootDepartmentID && len(user.DepartmentOpenIDs) == 0 {
					user.DepartmentOpenIDs = []string{departmentID}
					if user.PrimaryOpenDepartmentID == "" {
						user.PrimaryOpenDepartmentID = departmentID
					}
				}
				existing, ok := seenUsers[user.OpenID]
				if ok {
					user.DepartmentOpenIDs = uniqueSortedStrings(append(existing.DepartmentOpenIDs, user.DepartmentOpenIDs...))
					if user.PrimaryOpenDepartmentID == "" {
						user.PrimaryOpenDepartmentID = existing.PrimaryOpenDepartmentID
					}
				}
				seenUsers[user.OpenID] = user
			}
			if !out.Data.HasMore || strings.TrimSpace(out.Data.PageToken) == "" {
				break
			}
			pageToken = strings.TrimSpace(out.Data.PageToken)
		}
	}
	users := make([]FeishuOrgDirectoryUser, 0, len(seenUsers))
	for _, user := range seenUsers {
		users = append(users, user)
	}
	sort.Slice(users, func(i, j int) bool {
		return users[i].OpenID < users[j].OpenID
	})
	return users, nil
}

func (c *FeishuOrgDirectoryHTTPClient) doJSON(ctx context.Context, method, path string, query url.Values, bearerToken string, body io.Reader, dest any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, feishuOrgSyncResponseBodySize))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("feishu api status %d: %s", resp.StatusCode, truncateFeishuOrgSyncLogValue(string(raw), 1024))
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("decode feishu api response: %w", err)
	}
	return nil
}

func (s *FeishuOrgPermissionService) RunFeishuOrgSync(ctx context.Context, actorUserID int64, cfg config.FeishuConnectConfig, policy FeishuDeparturePolicy) (*FeishuManualReconcileResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	if !cfg.OrgSyncEnabled {
		return nil, infraerrors.BadRequest("FEISHU_ORG_SYNC_DISABLED", "feishu org sync is disabled")
	}
	if cfg.TenantRestrictionPolicy != FeishuTenantRestrictionInternalOnly {
		return nil, infraerrors.BadRequest("FEISHU_ORG_SYNC_REQUIRES_INTERNAL", "feishu org sync requires internal tenant policy")
	}
	client, err := NewFeishuOrgDirectoryHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return s.RunFeishuOrgSyncWithClient(ctx, actorUserID, client, policy)
}

func (s *FeishuOrgPermissionService) RunFeishuOrgSyncWithClient(ctx context.Context, actorUserID int64, client FeishuOrgDirectoryClient, policy FeishuDeparturePolicy) (*FeishuManualReconcileResult, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("feishu org permission service is not configured")
	}
	if client == nil {
		return nil, errors.New("feishu org directory client is not configured")
	}
	previousActiveUsers, err := s.countActiveFeishuOrgUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("count active feishu org users: %w", err)
	}
	snapshot, err := client.FetchSnapshot(ctx)
	if err != nil {
		_, _ = s.insertDetailedSyncRun(ctx, "failed", feishuOrgSyncImportCounts{}, 0, false, err.Error(), actorUserID)
		return nil, err
	}
	snapshot = normalizeFeishuDirectorySnapshot(snapshot)
	if snapshot == nil || strings.TrimSpace(snapshot.TenantKey) == "" {
		err := errors.New("feishu org snapshot missing tenant key")
		_, _ = s.insertDetailedSyncRun(ctx, "failed", feishuOrgSyncImportCounts{}, 0, false, err.Error(), actorUserID)
		return nil, err
	}
	if len(snapshot.Users) == 0 {
		err := errors.New("feishu org sync returned no users")
		_, _ = s.insertDetailedSyncRun(ctx, "failed", feishuOrgSyncImportCounts{}, 0, false, err.Error(), actorUserID)
		return nil, err
	}
	counts, err := s.importFeishuOrgSnapshot(ctx, snapshot)
	if err != nil {
		_, _ = s.insertDetailedSyncRun(ctx, "failed", counts, 0, false, err.Error(), actorUserID)
		return nil, err
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
	run, err := s.insertDetailedSyncRun(ctx, status, counts, len(departedUserIDs), reviewRequired, errorMessage, actorUserID)
	if err != nil {
		return nil, fmt.Errorf("insert sync run: %w", err)
	}
	return &FeishuManualReconcileResult{SyncRun: *run, Decision: decision}, nil
}

func (s *FeishuOrgPermissionService) importFeishuOrgSnapshot(ctx context.Context, snapshot *FeishuOrgDirectorySnapshot) (feishuOrgSyncImportCounts, error) {
	counts := feishuOrgSyncImportCounts{}
	for _, department := range snapshot.Departments {
		if err := s.upsertFeishuDepartment(ctx, department); err != nil {
			return counts, err
		}
		counts.DepartmentsSynced++
	}

	seenOpenIDs := make([]string, 0, len(snapshot.Users))
	for _, user := range snapshot.Users {
		if err := s.upsertFeishuOrgUser(ctx, user); err != nil {
			return counts, err
		}
		if err := s.replaceFeishuUserDepartments(ctx, user); err != nil {
			return counts, err
		}
		seenOpenIDs = append(seenOpenIDs, user.OpenID)
		counts.UsersSynced++
	}
	if len(seenOpenIDs) > 0 {
		if err := s.markMissingFeishuUsersDeparted(ctx, snapshot.TenantKey, seenOpenIDs); err != nil {
			return counts, err
		}
	}
	if len(snapshot.Departments) > 0 {
		seenDepartments := make([]string, 0, len(snapshot.Departments))
		for _, department := range snapshot.Departments {
			seenDepartments = append(seenDepartments, department.OpenDepartmentID)
		}
		if err := s.markMissingFeishuDepartmentsDeleted(ctx, snapshot.TenantKey, seenDepartments); err != nil {
			return counts, err
		}
	}
	managerCount, err := s.replaceFeishuDepartmentManagers(ctx, snapshot.TenantKey, snapshot.Departments)
	if err != nil {
		return counts, err
	}
	counts.ManagersSynced = managerCount
	bindingsMissing, err := s.countFeishuBindingsMissing(ctx, snapshot.TenantKey)
	if err != nil {
		return counts, err
	}
	counts.BindingsMissing = bindingsMissing
	return counts, nil
}

func (s *FeishuOrgPermissionService) upsertFeishuDepartment(ctx context.Context, department FeishuOrgDirectoryDepartment) error {
	leaderJSON, err := json.Marshal(uniqueSortedStrings(department.LeaderOpenIDs))
	if err != nil {
		return err
	}
	rawJSON, err := json.Marshal(nonNilMap(department.Raw))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO feishu_departments (
    tenant_key,
    open_department_id,
    parent_open_department_id,
    name,
    path,
    leader_open_ids,
    status,
    raw,
    last_synced_at,
    updated_at
)
VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8::jsonb, NOW(), NOW())
ON CONFLICT (tenant_key, open_department_id) DO UPDATE SET
    parent_open_department_id = EXCLUDED.parent_open_department_id,
    name = EXCLUDED.name,
    path = EXCLUDED.path,
    leader_open_ids = EXCLUDED.leader_open_ids,
    status = EXCLUDED.status,
    raw = EXCLUDED.raw,
    last_synced_at = NOW(),
    updated_at = NOW()`,
		department.TenantKey,
		department.OpenDepartmentID,
		department.ParentOpenDepartmentID,
		department.Name,
		department.Path,
		string(leaderJSON),
		normalizeFeishuDepartmentStatus(department.Status),
		string(rawJSON),
	)
	return err
}

func (s *FeishuOrgPermissionService) upsertFeishuOrgUser(ctx context.Context, user FeishuOrgDirectoryUser) error {
	departmentsJSON, err := json.Marshal(uniqueSortedStrings(user.DepartmentOpenIDs))
	if err != nil {
		return err
	}
	rawJSON, err := json.Marshal(nonNilMap(user.Raw))
	if err != nil {
		return err
	}
	identitySubjects := uniqueSortedStrings([]string{user.OpenID, user.UnionID, user.FeishuUserID})
	_, err = s.db.ExecContext(ctx, `
WITH matched_identity AS (
    SELECT user_id
    FROM auth_identities
    WHERE provider_type = 'feishu'
      AND provider_subject = ANY($13)
      AND (provider_key = $1 OR provider_key = '')
    ORDER BY CASE WHEN provider_key = $1 THEN 0 ELSE 1 END, id
    LIMIT 1
)
INSERT INTO feishu_org_users (
    user_id,
    tenant_key,
    open_id,
    union_id,
    feishu_user_id,
    name,
    email,
    employee_no,
    status,
    primary_open_department_id,
    manager_open_id,
    department_open_ids,
    raw,
    last_synced_at,
    updated_at
)
VALUES ((SELECT user_id FROM matched_identity), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12::jsonb, NOW(), NOW())
ON CONFLICT (tenant_key, open_id) DO UPDATE SET
    user_id = COALESCE((SELECT user_id FROM matched_identity), feishu_org_users.user_id),
    union_id = EXCLUDED.union_id,
    feishu_user_id = EXCLUDED.feishu_user_id,
    name = EXCLUDED.name,
    email = EXCLUDED.email,
    employee_no = EXCLUDED.employee_no,
    status = EXCLUDED.status,
    primary_open_department_id = EXCLUDED.primary_open_department_id,
    manager_open_id = EXCLUDED.manager_open_id,
    department_open_ids = EXCLUDED.department_open_ids,
    raw = EXCLUDED.raw,
    last_synced_at = NOW(),
    updated_at = NOW()`,
		user.TenantKey,
		user.OpenID,
		user.UnionID,
		user.FeishuUserID,
		user.Name,
		user.Email,
		user.EmployeeNo,
		normalizeFeishuUserStatus(user.Status),
		user.PrimaryOpenDepartmentID,
		user.ManagerOpenID,
		string(departmentsJSON),
		string(rawJSON),
		pq.Array(identitySubjects),
	)
	return err
}

func (s *FeishuOrgPermissionService) replaceFeishuUserDepartments(ctx context.Context, user FeishuOrgDirectoryUser) error {
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM feishu_user_departments
WHERE tenant_key = $1
  AND open_id = $2`, user.TenantKey, user.OpenID); err != nil {
		return err
	}
	for _, departmentID := range uniqueSortedStrings(user.DepartmentOpenIDs) {
		_, err := s.db.ExecContext(ctx, `
INSERT INTO feishu_user_departments (
    tenant_key,
    open_id,
    open_department_id,
    user_id,
    is_primary,
    updated_at
)
VALUES (
    $1,
    $2,
    $3,
    (SELECT user_id FROM feishu_org_users WHERE tenant_key = $1 AND open_id = $2),
    $4,
    NOW()
)
ON CONFLICT (tenant_key, open_id, open_department_id) DO UPDATE SET
    user_id = EXCLUDED.user_id,
    is_primary = EXCLUDED.is_primary,
    updated_at = NOW()`, user.TenantKey, user.OpenID, departmentID, departmentID == user.PrimaryOpenDepartmentID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *FeishuOrgPermissionService) replaceFeishuDepartmentManagers(ctx context.Context, tenantKey string, departments []FeishuOrgDirectoryDepartment) (int, error) {
	if _, err := s.db.ExecContext(ctx, `
UPDATE feishu_department_managers
SET status = 'disabled',
    updated_at = NOW()
WHERE tenant_key = $1
  AND source = 'feishu'`, tenantKey); err != nil {
		return 0, err
	}
	count := 0
	for _, department := range departments {
		for _, managerOpenID := range uniqueSortedStrings(department.LeaderOpenIDs) {
			_, err := s.db.ExecContext(ctx, `
INSERT INTO feishu_department_managers (
    tenant_key,
    open_department_id,
    manager_open_id,
    manager_user_id,
    source,
    relation_type,
    include_subdepartments,
    status,
    updated_at
)
VALUES (
    $1,
    $2,
    $3,
    (SELECT user_id FROM feishu_org_users WHERE tenant_key = $1 AND open_id = $3),
    'feishu',
    'department_leader',
    true,
    'active',
    NOW()
)
ON CONFLICT (tenant_key, open_department_id, manager_open_id) DO UPDATE SET
    manager_user_id = EXCLUDED.manager_user_id,
    source = 'feishu',
    relation_type = 'department_leader',
    include_subdepartments = true,
    status = 'active',
    updated_at = NOW()`, tenantKey, department.OpenDepartmentID, managerOpenID)
			if err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}

func (s *FeishuOrgPermissionService) markMissingFeishuUsersDeparted(ctx context.Context, tenantKey string, seenOpenIDs []string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE feishu_org_users
SET status = 'departed',
    last_synced_at = NOW(),
    updated_at = NOW()
WHERE tenant_key = $1
  AND status = 'active'
  AND NOT (open_id = ANY($2))`, tenantKey, pq.Array(uniqueSortedStrings(seenOpenIDs)))
	return err
}

func (s *FeishuOrgPermissionService) markMissingFeishuDepartmentsDeleted(ctx context.Context, tenantKey string, seenDepartmentIDs []string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE feishu_departments
SET status = 'deleted',
    last_synced_at = NOW(),
    updated_at = NOW()
WHERE tenant_key = $1
  AND status = 'active'
  AND NOT (open_department_id = ANY($2))`, tenantKey, pq.Array(uniqueSortedStrings(seenDepartmentIDs)))
	return err
}

func (s *FeishuOrgPermissionService) countFeishuBindingsMissing(ctx context.Context, tenantKey string) (int, error) {
	var count int
	err := scanFeishuSingleRow(ctx, s.db, `
SELECT COUNT(*)
FROM feishu_org_users
WHERE tenant_key = $1
  AND status = 'active'
  AND user_id IS NULL`, []any{tenantKey}, &count)
	return count, err
}

func (s *FeishuOrgPermissionService) insertDetailedSyncRun(ctx context.Context, status string, counts feishuOrgSyncImportCounts, usersToDisable int, reviewRequired bool, errorMessage string, actorUserID int64) (*FeishuOrgSyncRunView, error) {
	var item FeishuOrgSyncRunView
	var finishedAt sql.NullTime
	var triggeredBy sql.NullInt64
	err := scanFeishuSingleRow(ctx, s.db, `
INSERT INTO feishu_org_sync_runs (
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
)
VALUES ($1, NOW(), NOW(), $2, $3, $4, 0, $5, $6, $7, $8, NULLIF($9, 0))
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
          triggered_by_user_id`, []any{
		status,
		counts.DepartmentsSynced,
		counts.UsersSynced,
		counts.ManagersSynced,
		usersToDisable,
		counts.BindingsMissing,
		reviewRequired,
		errorMessage,
		actorUserID,
	},
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

func normalizeFeishuDirectorySnapshot(snapshot *FeishuOrgDirectorySnapshot) *FeishuOrgDirectorySnapshot {
	if snapshot == nil {
		return nil
	}
	snapshot.TenantKey = strings.TrimSpace(snapshot.TenantKey)
	if snapshot.FetchedAt.IsZero() {
		snapshot.FetchedAt = time.Now().UTC()
	}
	snapshot.Departments = normalizeFeishuDirectoryDepartments(snapshot.TenantKey, snapshot.Departments)
	snapshot.Users = normalizeFeishuDirectoryUsers(snapshot.TenantKey, snapshot.Users)
	return snapshot
}

func normalizeFeishuDirectoryDepartments(tenantKey string, departments []FeishuOrgDirectoryDepartment) []FeishuOrgDirectoryDepartment {
	departmentsByID := make(map[string]FeishuOrgDirectoryDepartment)
	for _, department := range departments {
		department.TenantKey = firstNonEmpty(strings.TrimSpace(department.TenantKey), tenantKey)
		department.OpenDepartmentID = strings.TrimSpace(department.OpenDepartmentID)
		if department.OpenDepartmentID == "" || department.OpenDepartmentID == feishuRootDepartmentID {
			continue
		}
		department.ParentOpenDepartmentID = strings.TrimSpace(department.ParentOpenDepartmentID)
		department.Name = strings.TrimSpace(department.Name)
		if department.Name == "" {
			department.Name = department.OpenDepartmentID
		}
		department.Status = normalizeFeishuDepartmentStatus(department.Status)
		department.LeaderOpenIDs = uniqueSortedStrings(department.LeaderOpenIDs)
		departmentsByID[department.OpenDepartmentID] = department
	}
	out := make([]FeishuOrgDirectoryDepartment, 0, len(departmentsByID))
	for id, department := range departmentsByID {
		if strings.TrimSpace(department.Path) == "" {
			department.Path = buildFeishuDepartmentPath(id, departmentsByID, nil)
		}
		out = append(out, department)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OpenDepartmentID < out[j].OpenDepartmentID
	})
	return out
}

func normalizeFeishuDirectoryUsers(tenantKey string, users []FeishuOrgDirectoryUser) []FeishuOrgDirectoryUser {
	usersByOpenID := make(map[string]FeishuOrgDirectoryUser)
	for _, user := range users {
		user.TenantKey = firstNonEmpty(strings.TrimSpace(user.TenantKey), tenantKey)
		user.OpenID = strings.TrimSpace(user.OpenID)
		if user.OpenID == "" {
			continue
		}
		user.UnionID = strings.TrimSpace(user.UnionID)
		user.FeishuUserID = strings.TrimSpace(user.FeishuUserID)
		user.Name = strings.TrimSpace(user.Name)
		if user.Name == "" {
			user.Name = firstNonEmpty(user.Email, user.OpenID)
		}
		user.Email = strings.TrimSpace(user.Email)
		user.EmployeeNo = strings.TrimSpace(user.EmployeeNo)
		user.Status = normalizeFeishuUserStatus(user.Status)
		user.DepartmentOpenIDs = uniqueSortedStrings(user.DepartmentOpenIDs)
		if user.PrimaryOpenDepartmentID == "" {
			user.PrimaryOpenDepartmentID = firstString(user.DepartmentOpenIDs)
		}
		usersByOpenID[user.OpenID] = user
	}
	out := make([]FeishuOrgDirectoryUser, 0, len(usersByOpenID))
	for _, user := range usersByOpenID {
		out = append(out, user)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].OpenID < out[j].OpenID
	})
	return out
}

func buildFeishuDepartmentPath(departmentID string, departments map[string]FeishuOrgDirectoryDepartment, visiting map[string]bool) string {
	department, ok := departments[departmentID]
	if !ok {
		return departmentID
	}
	if visiting == nil {
		visiting = make(map[string]bool)
	}
	if visiting[departmentID] {
		return department.Name
	}
	visiting[departmentID] = true
	parentID := strings.TrimSpace(department.ParentOpenDepartmentID)
	if parentID == "" || parentID == feishuRootDepartmentID {
		return department.Name
	}
	parentPath := buildFeishuDepartmentPath(parentID, departments, visiting)
	if parentPath == "" {
		return department.Name
	}
	return parentPath + "/" + department.Name
}

func parseFeishuDirectoryDepartment(raw map[string]any) FeishuOrgDirectoryDepartment {
	departmentID := firstNonEmpty(
		feishuMapString(raw, "open_department_id"),
		feishuMapString(raw, "department_id"),
		feishuMapString(raw, "id"),
	)
	parentID := firstNonEmpty(
		feishuMapString(raw, "parent_open_department_id"),
		feishuMapString(raw, "parent_department_id"),
	)
	return FeishuOrgDirectoryDepartment{
		OpenDepartmentID:       departmentID,
		ParentOpenDepartmentID: parentID,
		Name:                   firstNonEmpty(feishuMapString(raw, "name"), feishuMapString(raw, "department_name")),
		Status:                 parseFeishuDepartmentStatus(raw),
		LeaderOpenIDs:          parseFeishuLeaderOpenIDs(raw),
		Raw:                    raw,
	}
}

func parseFeishuDirectoryUser(raw map[string]any) FeishuOrgDirectoryUser {
	openID := firstNonEmpty(
		feishuMapString(raw, "open_id"),
		feishuMapString(raw, "user_id"),
		feishuMapString(raw, "id"),
	)
	departmentIDs := firstNonEmptyStringSlice(
		feishuMapStringSlice(raw, "department_open_ids"),
		feishuMapStringSlice(raw, "department_ids"),
	)
	return FeishuOrgDirectoryUser{
		OpenID:                  openID,
		UnionID:                 feishuMapString(raw, "union_id"),
		FeishuUserID:            firstNonEmpty(feishuMapString(raw, "user_id"), openID),
		Name:                    firstNonEmpty(feishuMapString(raw, "name"), feishuMapString(raw, "en_name")),
		Email:                   firstNonEmpty(feishuMapString(raw, "email"), feishuMapString(raw, "enterprise_email")),
		EmployeeNo:              firstNonEmpty(feishuMapString(raw, "employee_no"), feishuMapString(raw, "employee_id")),
		Status:                  parseFeishuUserStatus(raw),
		PrimaryOpenDepartmentID: firstNonEmpty(feishuMapString(raw, "primary_department_id"), firstString(departmentIDs)),
		ManagerOpenID:           firstNonEmpty(feishuMapString(raw, "leader_user_id"), feishuMapString(raw, "manager_open_id")),
		DepartmentOpenIDs:       departmentIDs,
		Raw:                     raw,
	}
}

func parseFeishuDepartmentStatus(raw map[string]any) string {
	if strings.EqualFold(feishuMapString(raw, "status"), "deleted") || feishuMapBool(raw, "is_deleted") {
		return "deleted"
	}
	return "active"
}

func parseFeishuUserStatus(raw map[string]any) string {
	if status, ok := raw["status"].(map[string]any); ok {
		if feishuMapBool(status, "is_resigned") || feishuMapBool(status, "is_exited") {
			return "departed"
		}
		if feishuMapBool(status, "is_frozen") || feishuMapBool(status, "is_suspended") {
			return "disabled"
		}
	}
	status := strings.ToLower(strings.TrimSpace(feishuMapString(raw, "status")))
	switch status {
	case "departed", "resigned", "exited":
		return "departed"
	case "disabled", "frozen", "suspended":
		return "disabled"
	default:
		return "active"
	}
}

func parseFeishuLeaderOpenIDs(raw map[string]any) []string {
	ids := []string{
		feishuMapString(raw, "leader_open_id"),
		feishuMapString(raw, "leader_user_id"),
		feishuMapString(raw, "department_leader_user_id"),
	}
	ids = append(ids, feishuMapStringSlice(raw, "leader_open_ids")...)
	ids = append(ids, feishuMapStringSlice(raw, "leader_user_ids")...)
	if leaders, ok := raw["leaders"].([]any); ok {
		for _, value := range leaders {
			if leader, ok := value.(map[string]any); ok {
				ids = append(ids,
					feishuMapString(leader, "open_id"),
					feishuMapString(leader, "user_id"),
					feishuMapString(leader, "leader_id"),
					feishuMapString(leader, "leaderID"),
				)
			}
		}
	}
	return uniqueSortedStrings(ids)
}

func normalizeFeishuDepartmentStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "deleted":
		return "deleted"
	default:
		return "active"
	}
}

func normalizeFeishuUserStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "departed", "resigned", "exited":
		return "departed"
	case "disabled", "frozen", "suspended":
		return "disabled"
	default:
		return "active"
	}
}

func feishuMapString(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	value, ok := raw[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", typed))
	}
}

func feishuMapBool(raw map[string]any, key string) bool {
	if raw == nil {
		return false
	}
	value, ok := raw[key]
	if !ok || value == nil {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func feishuMapStringSlice(raw map[string]any, key string) []string {
	if raw == nil {
		return nil
	}
	value, ok := raw[key]
	if !ok || value == nil {
		return nil
	}
	switch typed := value.(type) {
	case []string:
		return uniqueSortedStrings(typed)
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if item == nil {
				continue
			}
			out = append(out, strings.TrimSpace(fmt.Sprintf("%v", item)))
		}
		return uniqueSortedStrings(out)
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	default:
		return nil
	}
}

func firstNonEmptyStringSlice(values ...[]string) []string {
	for _, value := range values {
		if len(uniqueSortedStrings(value)) > 0 {
			return uniqueSortedStrings(value)
		}
	}
	return nil
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{})
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		seen[trimmed] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func truncateFeishuOrgSyncLogValue(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}
