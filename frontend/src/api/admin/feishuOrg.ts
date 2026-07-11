import { apiClient } from '../client'
import type {
  GroupStat,
  ModelStat,
  PaginatedResponse,
  TrendDataPoint,
  UsageLog,
  UsageQueryParams,
  UsageStatsResponse,
} from '@/types'
import type { UsageDashboardSnapshotV2Response, UserDashboardStats } from '@/api/usage'

const FEISHU_ORG_MANUAL_SYNC_TIMEOUT_MS = 5 * 60 * 1000

export interface FeishuOrgGroupBrief {
  id: number
  name: string
  platform: string
  subscription_type: string
}

export interface FeishuOrgDepartment {
  tenant_key: string
  open_department_id: string
  parent_open_department_id: string
  name: string
  path: string
  status: string
  leader_open_ids: string[]
  employee_count: number
  manager_count: number
  assignable_groups: FeishuOrgGroupBrief[]
  last_synced_at?: string | null
}

export interface FeishuOrgUser {
  user_id: number
  local_email: string
  local_username: string
  local_status: string
  tenant_key: string
  open_id: string
  union_id: string
  feishu_user_id: string
  name: string
  email: string
  employee_no: string
  status: string
  primary_open_department_id: string
  primary_department_name: string
  primary_department_path: string
  department_open_ids: string[]
  department_manager_group_ids: number[]
  super_admin_override_group_ids: number[]
  effective_group_ids: number[]
  assignable_groups?: FeishuOrgGroupBrief[]
  last_synced_at?: string | null
}

export interface FeishuOrgSyncRun {
  id: number
  status: string
  started_at: string
  finished_at?: string | null
  departments_synced: number
  users_synced: number
  managers_synced: number
  users_to_create: number
  users_to_disable: number
  bindings_missing: number
  review_required: boolean
  error_message: string
  triggered_by_user_id: number
}

export interface FeishuOrgListResult<T> {
  items: T[]
  total: number
  limit: number
  offset: number
}

export interface FeishuOrgListParams {
  tenant_key?: string
  q?: string
  limit?: number
  offset?: number
}

export interface FeishuOrgSetGroupsRequest {
  tenant_key?: string
  group_ids: number[]
  reason?: string
}

export interface FeishuOrgManagerAccess {
  has_access: boolean
}

export type FeishuOrgUsageQueryParams = UsageQueryParams & {
  sort_by?: string
  sort_order?: 'asc' | 'desc'
}

export interface FeishuOrgTrendResponse {
  trend: TrendDataPoint[]
  start_date: string
  end_date: string
  granularity: string
}

export interface FeishuOrgModelStatsResponse {
  models: ModelStat[]
  start_date: string
  end_date: string
}

export interface FeishuOrgGroupStatsResponse {
  groups: GroupStat[]
  start_date: string
  end_date: string
}

export async function listDepartments(params?: FeishuOrgListParams): Promise<FeishuOrgListResult<FeishuOrgDepartment>> {
  const { data } = await apiClient.get<FeishuOrgListResult<FeishuOrgDepartment>>('/admin/feishu-org/departments', {
    params
  })
  return data
}

export async function listUsers(params?: FeishuOrgListParams): Promise<FeishuOrgListResult<FeishuOrgUser>> {
  const { data } = await apiClient.get<FeishuOrgListResult<FeishuOrgUser>>('/admin/feishu-org/users', {
    params
  })
  return data
}

export async function listSyncRuns(params?: FeishuOrgListParams): Promise<FeishuOrgListResult<FeishuOrgSyncRun>> {
  const { data } = await apiClient.get<FeishuOrgListResult<FeishuOrgSyncRun>>('/admin/feishu-org/sync-runs', {
    params
  })
  return data
}

export async function runManualReconcile() {
  const { data } = await apiClient.post('/admin/feishu-org/sync-runs', undefined, {
    timeout: FEISHU_ORG_MANUAL_SYNC_TIMEOUT_MS
  })
  return data
}

export async function setDepartmentGroupPool(
  departmentId: string,
  payload: FeishuOrgSetGroupsRequest
) {
  const { data } = await apiClient.put(`/admin/feishu-org/departments/${encodeURIComponent(departmentId)}/groups`, payload)
  return data
}

export async function setUserOverrideGroupGrants(userId: number, payload: FeishuOrgSetGroupsRequest) {
  const { data } = await apiClient.put(`/admin/feishu-org/users/${userId}/overrides`, payload)
  return data
}

export async function listManagedUsers(params?: FeishuOrgListParams): Promise<FeishuOrgListResult<FeishuOrgUser>> {
  const { data } = await apiClient.get<FeishuOrgListResult<FeishuOrgUser>>('/org-manager/users', {
    params
  })
  return data
}

export async function getManagerAccess(): Promise<FeishuOrgManagerAccess> {
  const { data } = await apiClient.get<FeishuOrgManagerAccess>('/org-manager/access')
  return data
}

export async function setManagedUserGroupGrants(userId: number, payload: FeishuOrgSetGroupsRequest) {
  const { data } = await apiClient.put(`/org-manager/users/${userId}/group-grants`, payload)
  return data
}

export async function queryManagerUsage(
  params: FeishuOrgUsageQueryParams,
  config: { signal?: AbortSignal } = {}
): Promise<PaginatedResponse<UsageLog>> {
  const { data } = await apiClient.get<PaginatedResponse<UsageLog>>('/org-manager/usage', {
    ...config,
    params
  })
  return data
}

export async function getManagerUsageStats(params: UsageQueryParams): Promise<UsageStatsResponse> {
  const { data } = await apiClient.get<UsageStatsResponse>('/org-manager/usage/stats', {
    params
  })
  return data
}

export async function getManagerDashboardStats(params?: UsageQueryParams): Promise<UserDashboardStats> {
  const { data } = await apiClient.get<UserDashboardStats>('/org-manager/usage/dashboard/stats', {
    params
  })
  return data
}

export async function getManagerUsageTrend(params: UsageQueryParams & { granularity?: 'day' | 'hour' }): Promise<FeishuOrgTrendResponse> {
  const { data } = await apiClient.get<FeishuOrgTrendResponse>('/org-manager/usage/dashboard/trend', {
    params
  })
  return data
}

export async function getManagerUsageModels(params: UsageQueryParams & { model_source?: 'requested' }): Promise<FeishuOrgModelStatsResponse> {
  const { data } = await apiClient.get<FeishuOrgModelStatsResponse>('/org-manager/usage/dashboard/models', {
    params
  })
  return data
}

export async function getManagerUsageSnapshotV2(
  params: UsageQueryParams & {
    granularity?: 'day' | 'hour'
    include_trend?: boolean
    include_model_stats?: boolean
    include_group_stats?: boolean
  }
): Promise<UsageDashboardSnapshotV2Response> {
  const { data } = await apiClient.get<UsageDashboardSnapshotV2Response>('/org-manager/usage/dashboard/snapshot-v2', {
    params
  })
  return data
}

export default {
  listDepartments,
  listUsers,
  listSyncRuns,
  runManualReconcile,
  setDepartmentGroupPool,
  setUserOverrideGroupGrants,
  listManagedUsers,
  getManagerAccess,
  setManagedUserGroupGrants,
  queryManagerUsage,
  getManagerUsageStats,
  getManagerDashboardStats,
  getManagerUsageTrend,
  getManagerUsageModels,
  getManagerUsageSnapshotV2,
}
