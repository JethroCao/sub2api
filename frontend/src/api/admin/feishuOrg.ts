import { apiClient } from '../client'

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
  limit: number
  offset: number
}

export interface FeishuOrgListParams {
  tenant_key?: string
  limit?: number
  offset?: number
}

export interface FeishuOrgSetGroupsRequest {
  tenant_key?: string
  group_ids: number[]
  reason?: string
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
  const { data } = await apiClient.post('/admin/feishu-org/sync-runs')
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

export async function setManagedUserGroupGrants(userId: number, payload: FeishuOrgSetGroupsRequest) {
  const { data } = await apiClient.put(`/org-manager/users/${userId}/group-grants`, payload)
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
  setManagedUserGroupGrants,
}
