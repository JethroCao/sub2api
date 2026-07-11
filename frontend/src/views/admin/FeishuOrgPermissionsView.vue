<template>
  <AppLayout>
    <div class="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <div class="mb-5 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">飞书组织权限</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            按飞书同步的部门维护可分配分组，并处理员工个人额外授权。
          </p>
        </div>
        <div class="flex gap-2">
          <button v-if="activeTab === 'sync'" class="btn btn-primary" :disabled="reconciling" @click="runManualReconcile">
            {{ reconciling ? '同步中' : '立即同步飞书' }}
          </button>
          <button class="btn btn-secondary" :disabled="loading" @click="reloadCurrentTab">
            {{ loading ? '刷新中' : '刷新' }}
          </button>
        </div>
      </div>

      <div class="mb-4 flex flex-wrap gap-2">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          class="rounded-lg px-4 py-2 text-sm font-medium transition-colors"
          :class="activeTab === tab.key
            ? 'bg-primary-600 text-white shadow-sm'
            : 'bg-white text-gray-600 ring-1 ring-gray-200 hover:bg-gray-50 dark:bg-dark-800 dark:text-gray-300 dark:ring-dark-700 dark:hover:bg-dark-700'"
          @click="switchTab(tab.key)"
        >
          {{ tab.label }}
        </button>
      </div>

      <div v-if="activeTab === 'departments'" class="mb-4 flex flex-wrap items-center gap-2">
        <input
          v-model="departmentSearchDraft"
          type="search"
          class="input w-full sm:w-80"
          placeholder="搜索部门名称、路径或部门 ID"
          @keydown.enter.prevent="applyDepartmentSearch"
        />
        <button class="btn btn-secondary" :disabled="loading" @click="applyDepartmentSearch">搜索</button>
        <button v-if="departmentSearch" class="btn btn-ghost" :disabled="loading" @click="clearDepartmentSearch">清空</button>
      </div>

      <div v-if="activeTab === 'users'" class="mb-4 flex flex-wrap items-center gap-2">
        <input
          v-model="userSearchDraft"
          type="search"
          class="input w-full sm:w-80"
          placeholder="搜索姓名、邮箱、工号、部门或飞书 ID"
          @keydown.enter.prevent="applyUserSearch"
        />
        <button class="btn btn-secondary" :disabled="loading" @click="applyUserSearch">搜索</button>
        <button v-if="userSearch" class="btn btn-ghost" :disabled="loading" @click="clearUserSearch">清空</button>
      </div>

      <section v-if="activeTab === 'departments'" class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
        <div class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
              <tr>
                <th class="px-4 py-3">部门</th>
                <th class="px-4 py-3">人数</th>
                <th class="px-4 py-3">负责人</th>
                <th class="px-4 py-3">可分配分组</th>
                <th class="px-4 py-3 text-right">操作</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="dept in departments" :key="dept.tenant_key + ':' + dept.open_department_id">
                <td class="px-4 py-3">
                  <div class="font-medium text-gray-900 dark:text-white">{{ dept.name || dept.open_department_id }}</div>
                  <div class="mt-0.5 text-xs text-gray-500">{{ dept.path || dept.open_department_id }}</div>
                </td>
                <td class="px-4 py-3 text-gray-700 dark:text-gray-200">{{ dept.employee_count }}</td>
                <td class="px-4 py-3 text-gray-700 dark:text-gray-200">{{ dept.manager_count }}</td>
                <td class="px-4 py-3">
                  <div class="flex max-w-xl flex-wrap gap-1.5">
                    <span v-for="group in dept.assignable_groups" :key="group.id" class="rounded bg-emerald-50 px-2 py-1 text-xs text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-200">
                      {{ group.name }}
                    </span>
                    <span v-if="dept.assignable_groups.length === 0" class="text-xs text-gray-400">未配置</span>
                  </div>
                </td>
                <td class="px-4 py-3 text-right">
                  <button class="btn btn-sm btn-primary" @click="openDepartmentEditor(dept)">配置分组</button>
                </td>
              </tr>
              <tr v-if="!loading && departments.length === 0">
                <td colspan="5" class="px-4 py-8 text-center text-gray-500">暂无部门同步数据</td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="departmentPagination.total > 0"
          :page="departmentPagination.page"
          :total="departmentPagination.total"
          :page-size="departmentPagination.page_size"
          @update:page="(page) => handlePageChange(departmentPagination, loadDepartments, page)"
          @update:pageSize="(pageSize) => handlePageSizeChange(departmentPagination, loadDepartments, pageSize)"
        />
      </section>

      <section v-if="activeTab === 'users'" class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
        <div class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
              <tr>
                <th class="px-4 py-3">员工</th>
                <th class="px-4 py-3">主部门</th>
                <th class="px-4 py-3">部门授权</th>
                <th class="px-4 py-3">个人额外授权</th>
                <th class="px-4 py-3">最终分组</th>
                <th class="px-4 py-3 text-right">操作</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="user in users" :key="user.tenant_key + ':' + user.open_id">
                <td class="px-4 py-3">
                  <div class="font-medium text-gray-900 dark:text-white">{{ user.name || user.local_username || user.open_id }}</div>
                  <div class="mt-0.5 text-xs text-gray-500">{{ user.local_email || user.email || user.open_id }}</div>
                </td>
                <td class="px-4 py-3">
                  <div class="text-gray-700 dark:text-gray-200">{{ user.primary_department_name || user.primary_open_department_id || '-' }}</div>
                  <div v-if="user.department_open_ids.length > 1" class="mt-1 text-xs text-amber-600 dark:text-amber-300">
                    多部门，仅按主部门授权
                  </div>
                </td>
                <td class="px-4 py-3">{{ formatGroupIds(user.department_manager_group_ids) }}</td>
                <td class="px-4 py-3">{{ formatGroupIds(user.super_admin_override_group_ids) }}</td>
                <td class="px-4 py-3">{{ formatGroupIds(user.effective_group_ids) }}</td>
                <td class="px-4 py-3 text-right">
                  <button class="btn btn-sm btn-secondary" :disabled="user.user_id <= 0" @click="openOverrideEditor(user)">个人授权</button>
                </td>
              </tr>
              <tr v-if="!loading && users.length === 0">
                <td colspan="6" class="px-4 py-8 text-center text-gray-500">暂无员工同步数据</td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="userPagination.total > 0"
          :page="userPagination.page"
          :total="userPagination.total"
          :page-size="userPagination.page_size"
          @update:page="(page) => handlePageChange(userPagination, loadUsers, page)"
          @update:pageSize="(pageSize) => handlePageSizeChange(userPagination, loadUsers, pageSize)"
        />
      </section>

      <section v-if="activeTab === 'sync'" class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
        <div class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
              <tr>
                <th class="px-4 py-3">时间</th>
                <th class="px-4 py-3">状态</th>
                <th class="px-4 py-3">部门/员工/负责人</th>
                <th class="px-4 py-3">待创建/待禁用/缺绑定</th>
                <th class="px-4 py-3">复核</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="run in syncRuns" :key="run.id">
                <td class="px-4 py-3 text-gray-700 dark:text-gray-200">{{ formatDate(run.started_at) }}</td>
                <td class="px-4 py-3">
                  <span class="rounded px-2 py-1 text-xs" :class="run.status === 'success' ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-200' : 'bg-amber-50 text-amber-700 dark:bg-amber-900/30 dark:text-amber-200'">
                    {{ run.status }}
                  </span>
                </td>
                <td class="px-4 py-3 text-gray-700 dark:text-gray-200">{{ run.departments_synced }} / {{ run.users_synced }} / {{ run.managers_synced }}</td>
                <td class="px-4 py-3 text-gray-700 dark:text-gray-200">{{ run.users_to_create }} / {{ run.users_to_disable }} / {{ run.bindings_missing }}</td>
                <td class="px-4 py-3 text-gray-700 dark:text-gray-200">{{ run.review_required ? '需要' : '否' }}</td>
              </tr>
              <tr v-if="!loading && syncRuns.length === 0">
                <td colspan="5" class="px-4 py-8 text-center text-gray-500">暂无同步记录</td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="syncPagination.total > 0"
          :page="syncPagination.page"
          :total="syncPagination.total"
          :page-size="syncPagination.page_size"
          @update:page="(page) => handlePageChange(syncPagination, loadSyncRuns, page)"
          @update:pageSize="(pageSize) => handlePageSizeChange(syncPagination, loadSyncRuns, pageSize)"
        />
      </section>
    </div>

    <div v-if="editor.visible" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 px-4">
      <div class="w-full max-w-2xl rounded-lg bg-white shadow-xl dark:bg-dark-800">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ editor.title }}</h2>
          <button class="text-gray-400 hover:text-gray-600" @click="closeEditor">×</button>
        </div>
        <div class="max-h-[60vh] overflow-y-auto px-5 py-4">
          <div class="grid gap-2 sm:grid-cols-2">
            <label v-for="group in allGroups" :key="group.id" class="flex items-center gap-2 rounded border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
              <input v-model="selectedGroupIds" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600" :value="group.id" />
              <span class="min-w-0 flex-1 truncate text-gray-800 dark:text-gray-100">{{ group.name }}</span>
              <span class="text-xs text-gray-400">{{ group.platform }}</span>
            </label>
          </div>
        </div>
        <div class="flex justify-end gap-2 border-t border-gray-200 px-5 py-4 dark:border-dark-700">
          <button class="btn btn-secondary" @click="closeEditor">取消</button>
          <button class="btn btn-primary" :disabled="saving" @click="saveEditor">{{ saving ? '保存中' : '保存' }}</button>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import Pagination from '@/components/common/Pagination.vue'
import feishuOrgAPI, {
  type FeishuOrgDepartment,
  type FeishuOrgSyncRun,
  type FeishuOrgUser
} from '@/api/admin/feishuOrg'
import groupsAPI from '@/api/admin/groups'
import { useAppStore } from '@/stores'
import type { AdminGroup } from '@/types'

type TabKey = 'departments' | 'users' | 'sync'
type PagerState = { page: number; page_size: number; total: number }
type PageLoader = () => Promise<void>

const appStore = useAppStore()
const tabs: Array<{ key: TabKey; label: string }> = [
  { key: 'departments', label: '部门分组池' },
  { key: 'users', label: '员工授权' },
  { key: 'sync', label: '同步状态' }
]

const activeTab = ref<TabKey>('departments')
const loading = ref(false)
const saving = ref(false)
const reconciling = ref(false)
const departments = ref<FeishuOrgDepartment[]>([])
const users = ref<FeishuOrgUser[]>([])
const syncRuns = ref<FeishuOrgSyncRun[]>([])
const allGroups = ref<AdminGroup[]>([])
const selectedGroupIds = ref<number[]>([])
const departmentSearch = ref('')
const departmentSearchDraft = ref('')
const userSearch = ref('')
const userSearchDraft = ref('')
const departmentPagination = reactive<PagerState>({ page: 1, page_size: 20, total: 0 })
const userPagination = reactive<PagerState>({ page: 1, page_size: 20, total: 0 })
const syncPagination = reactive<PagerState>({ page: 1, page_size: 20, total: 0 })

const editor = reactive<{
  visible: boolean
  mode: 'department' | 'override' | null
  title: string
  department?: FeishuOrgDepartment
  user?: FeishuOrgUser
}>({
  visible: false,
  mode: null,
  title: ''
})

onMounted(() => {
  void Promise.all([loadGroups(), loadDepartments()])
})

async function switchTab(tab: TabKey) {
  activeTab.value = tab
  await reloadCurrentTab()
}

async function reloadCurrentTab() {
  if (activeTab.value === 'departments') return loadDepartments()
  if (activeTab.value === 'users') return loadUsers()
  return loadSyncRuns()
}

async function loadDepartments() {
  loading.value = true
  try {
    const result = await feishuOrgAPI.listDepartments({
      q: normalizeSearchParam(departmentSearch.value),
      limit: departmentPagination.page_size,
      offset: paginationOffset(departmentPagination)
    })
    departments.value = result.items || []
    departmentPagination.total = normalizeTotal(result.total, departments.value.length)
  } catch (error: any) {
    appStore.showError(error?.message || '加载飞书部门失败')
  } finally {
    loading.value = false
  }
}

async function loadUsers() {
  loading.value = true
  try {
    const result = await feishuOrgAPI.listUsers({
      q: normalizeSearchParam(userSearch.value),
      limit: userPagination.page_size,
      offset: paginationOffset(userPagination)
    })
    users.value = result.items || []
    userPagination.total = normalizeTotal(result.total, users.value.length)
  } catch (error: any) {
    appStore.showError(error?.message || '加载飞书员工失败')
  } finally {
    loading.value = false
  }
}

async function loadSyncRuns() {
  loading.value = true
  try {
    const result = await feishuOrgAPI.listSyncRuns({
      limit: syncPagination.page_size,
      offset: paginationOffset(syncPagination)
    })
    syncRuns.value = result.items || []
    syncPagination.total = normalizeTotal(result.total, syncRuns.value.length)
  } catch (error: any) {
    appStore.showError(error?.message || '加载同步状态失败')
  } finally {
    loading.value = false
  }
}

async function loadGroups() {
  try {
    allGroups.value = await groupsAPI.getAll()
  } catch (error: any) {
    appStore.showError(error?.message || '加载分组失败')
  }
}

async function runManualReconcile() {
  reconciling.value = true
  try {
    await feishuOrgAPI.runManualReconcile()
    appStore.showSuccess('飞书组织同步已完成')
    syncPagination.page = 1
    await loadSyncRuns()
  } catch (error: any) {
    appStore.showError(error?.message || '飞书组织同步失败')
  } finally {
    reconciling.value = false
  }
}

function openDepartmentEditor(department: FeishuOrgDepartment) {
  editor.visible = true
  editor.mode = 'department'
  editor.department = department
  editor.user = undefined
  editor.title = `配置部门分组：${department.name || department.open_department_id}`
  selectedGroupIds.value = department.assignable_groups.map((group) => group.id)
}

function openOverrideEditor(user: FeishuOrgUser) {
  editor.visible = true
  editor.mode = 'override'
  editor.user = user
  editor.department = undefined
  editor.title = `个人额外授权：${user.name || user.local_email || user.open_id}`
  selectedGroupIds.value = [...user.super_admin_override_group_ids]
}

function closeEditor() {
  editor.visible = false
  editor.mode = null
  editor.department = undefined
  editor.user = undefined
  selectedGroupIds.value = []
}

function applyDepartmentSearch() {
  departmentSearch.value = departmentSearchDraft.value.trim()
  departmentPagination.page = 1
  void loadDepartments()
}

function clearDepartmentSearch() {
  departmentSearchDraft.value = ''
  if (!departmentSearch.value) return
  departmentSearch.value = ''
  departmentPagination.page = 1
  void loadDepartments()
}

function applyUserSearch() {
  userSearch.value = userSearchDraft.value.trim()
  userPagination.page = 1
  void loadUsers()
}

function clearUserSearch() {
  userSearchDraft.value = ''
  if (!userSearch.value) return
  userSearch.value = ''
  userPagination.page = 1
  void loadUsers()
}

async function saveEditor() {
  saving.value = true
  try {
    if (editor.mode === 'department' && editor.department) {
      await feishuOrgAPI.setDepartmentGroupPool(editor.department.open_department_id, {
        tenant_key: editor.department.tenant_key,
        group_ids: selectedGroupIds.value,
        reason: 'admin_ui'
      })
      await loadDepartments()
    }
    if (editor.mode === 'override' && editor.user) {
      await feishuOrgAPI.setUserOverrideGroupGrants(editor.user.user_id, {
        group_ids: selectedGroupIds.value,
        reason: 'admin_ui'
      })
      await loadUsers()
    }
    appStore.showSuccess('保存成功')
    closeEditor()
  } catch (error: any) {
    appStore.showError(error?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

function formatGroupIds(ids: number[]) {
  if (!ids || ids.length === 0) return '-'
  return ids.map((id) => groupName(id)).join('、')
}

function groupName(id: number) {
  return allGroups.value.find((group) => group.id === id)?.name || `#${id}`
}

function formatDate(value?: string | null) {
  if (!value) return '-'
  return new Date(value).toLocaleString()
}

function paginationOffset(pagination: PagerState) {
  return (pagination.page - 1) * pagination.page_size
}

function normalizeSearchParam(value: string) {
  const trimmed = value.trim()
  return trimmed || undefined
}

function normalizeTotal(total: number | undefined, fallback: number) {
  return typeof total === 'number' ? total : fallback
}

function handlePageChange(pagination: PagerState, loader: PageLoader, page: number) {
  pagination.page = page
  void loader()
}

function handlePageSizeChange(pagination: PagerState, loader: PageLoader, pageSize: number) {
  pagination.page_size = pageSize
  pagination.page = 1
  void loader()
}
</script>
