<template>
  <AppLayout>
    <div class="mx-auto max-w-6xl px-4 py-6 sm:px-6 lg:px-8">
      <div class="mb-5 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 class="text-2xl font-semibold text-gray-900 dark:text-white">部门成员授权</h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            仅能管理自己负责范围内的员工授权与部门用量。
          </p>
        </div>
        <button class="btn btn-secondary" :disabled="currentLoading" @click="refreshCurrentTab">
          {{ currentLoading ? '刷新中' : '刷新' }}
        </button>
      </div>

      <div class="mb-4 flex flex-wrap items-center gap-2">
        <button
          v-for="tab in tabs"
          :key="tab.value"
          class="btn"
          :class="activeTab === tab.value ? 'btn-primary' : 'btn-secondary'"
          @click="switchTab(tab.value)"
        >
          {{ tab.label }}
        </button>
      </div>

      <div v-if="activeTab === 'members'" class="mb-4 flex flex-wrap items-center gap-2">
        <input
          v-model="searchDraft"
          type="search"
          class="input w-full sm:w-80"
          placeholder="搜索姓名、邮箱、工号、部门或飞书 ID"
          @keydown.enter.prevent="applySearch"
        />
        <button class="btn btn-secondary" :disabled="loading" @click="applySearch">搜索</button>
        <button v-if="search" class="btn btn-ghost" :disabled="loading" @click="clearSearch">清空</button>
      </div>

      <section v-if="activeTab === 'members'" class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
        <div class="overflow-x-auto">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
              <tr>
                <th class="px-4 py-3">员工</th>
                <th class="px-4 py-3">主部门</th>
                <th class="px-4 py-3">当前部门授权</th>
                <th class="px-4 py-3">可分配分组</th>
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
                  <div class="text-gray-700 dark:text-gray-200">{{ user.primary_department_name || user.primary_open_department_id }}</div>
                  <div v-if="user.department_open_ids.length > 1" class="mt-1 text-xs text-amber-600 dark:text-amber-300">
                    多部门，仅按主部门授权
                  </div>
                </td>
                <td class="px-4 py-3">{{ formatGroupIds(user.department_manager_group_ids, user.assignable_groups || []) }}</td>
                <td class="px-4 py-3">
                  <div class="flex max-w-lg flex-wrap gap-1.5">
                    <span v-for="group in user.assignable_groups || []" :key="group.id" class="rounded bg-emerald-50 px-2 py-1 text-xs text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-200">
                      {{ group.name }}
                    </span>
                    <span v-if="!user.assignable_groups?.length" class="text-xs text-gray-400">部门暂未授权可分配分组</span>
                  </div>
                </td>
                <td class="px-4 py-3 text-right">
                  <button class="btn btn-sm btn-primary" :disabled="!user.assignable_groups?.length" @click="openEditor(user)">分配</button>
                </td>
              </tr>
              <tr v-if="!loading && users.length === 0">
                <td colspan="5" class="px-4 py-8 text-center text-gray-500">暂无可管理的员工</td>
              </tr>
            </tbody>
          </table>
        </div>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </section>

      <section v-else-if="activeTab === 'summary'" class="space-y-4">
        <div class="flex flex-wrap items-center gap-3">
          <label class="text-sm font-medium text-gray-700 dark:text-gray-200">时间范围</label>
          <select v-model="usageRange" class="input w-40" @change="loadManagerUsageSummary">
            <option value="1d">今天</option>
            <option value="7d">近 7 天</option>
            <option value="30d">近 30 天</option>
          </select>
        </div>

        <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <div class="rounded-lg bg-white p-4 shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
            <div class="text-sm text-gray-500">总请求数</div>
            <div class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ formatNumber(usageStats?.total_requests) }}</div>
          </div>
          <div class="rounded-lg bg-white p-4 shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
            <div class="text-sm text-gray-500">总 Token</div>
            <div class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ formatNumber(usageStats?.total_tokens) }}</div>
          </div>
          <div class="rounded-lg bg-white p-4 shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
            <div class="text-sm text-gray-500">总消费</div>
            <div class="mt-2 text-2xl font-semibold text-emerald-600 dark:text-emerald-300">{{ formatCurrency(usageStats?.total_actual_cost) }}</div>
          </div>
          <div class="rounded-lg bg-white p-4 shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
            <div class="text-sm text-gray-500">平均耗时</div>
            <div class="mt-2 text-2xl font-semibold text-gray-900 dark:text-white">{{ formatDuration(usageStats?.average_duration_ms) }}</div>
          </div>
        </div>

        <div class="grid gap-4 lg:grid-cols-2">
          <div class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
            <div class="border-b border-gray-100 px-4 py-3 font-medium text-gray-900 dark:border-dark-700 dark:text-white">模型分布</div>
            <table class="min-w-full divide-y divide-gray-100 text-sm dark:divide-dark-700">
              <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
                <tr>
                  <th class="px-4 py-3">模型</th>
                  <th class="px-4 py-3 text-right">请求</th>
                  <th class="px-4 py-3 text-right">Token</th>
                  <th class="px-4 py-3 text-right">消费</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="model in modelStats" :key="model.model">
                  <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">{{ model.model || '-' }}</td>
                  <td class="px-4 py-3 text-right">{{ formatNumber(model.requests) }}</td>
                  <td class="px-4 py-3 text-right">{{ formatNumber(model.total_tokens) }}</td>
                  <td class="px-4 py-3 text-right">{{ formatCurrency(model.actual_cost) }}</td>
                </tr>
                <tr v-if="!usageLoading && modelStats.length === 0">
                  <td colspan="4" class="px-4 py-8 text-center text-gray-500">暂无数据</td>
                </tr>
              </tbody>
            </table>
          </div>

          <div class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
            <div class="border-b border-gray-100 px-4 py-3 font-medium text-gray-900 dark:border-dark-700 dark:text-white">分组分布</div>
            <table class="min-w-full divide-y divide-gray-100 text-sm dark:divide-dark-700">
              <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
                <tr>
                  <th class="px-4 py-3">分组</th>
                  <th class="px-4 py-3 text-right">请求</th>
                  <th class="px-4 py-3 text-right">Token</th>
                  <th class="px-4 py-3 text-right">消费</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="group in groupStats" :key="group.group_id">
                  <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">{{ group.group_name || `#${group.group_id}` }}</td>
                  <td class="px-4 py-3 text-right">{{ formatNumber(group.requests) }}</td>
                  <td class="px-4 py-3 text-right">{{ formatNumber(group.total_tokens) }}</td>
                  <td class="px-4 py-3 text-right">{{ formatCurrency(group.actual_cost) }}</td>
                </tr>
                <tr v-if="!usageLoading && groupStats.length === 0">
                  <td colspan="4" class="px-4 py-8 text-center text-gray-500">暂无数据</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </section>

      <section v-else class="space-y-4">
        <div class="flex flex-wrap items-center gap-3">
          <label class="text-sm font-medium text-gray-700 dark:text-gray-200">时间范围</label>
          <select v-model="usageRange" class="input w-40" @change="reloadManagerUsageLogs">
            <option value="1d">今天</option>
            <option value="7d">近 7 天</option>
            <option value="30d">近 30 天</option>
          </select>
        </div>
        <div class="overflow-hidden rounded-lg bg-white shadow-sm ring-1 ring-gray-200 dark:bg-dark-800 dark:ring-dark-700">
          <div class="overflow-x-auto">
            <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
              <thead class="bg-gray-50 text-left text-xs uppercase tracking-wide text-gray-500 dark:bg-dark-700/60 dark:text-gray-400">
                <tr>
                  <th class="px-4 py-3">员工</th>
                  <th class="px-4 py-3">模型</th>
                  <th class="px-4 py-3">分组</th>
                  <th class="px-4 py-3 text-right">Token</th>
                  <th class="px-4 py-3 text-right">消费</th>
                  <th class="px-4 py-3">时间</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="log in usageLogs" :key="log.id">
                  <td class="px-4 py-3">
                    <div class="font-medium text-gray-900 dark:text-white">{{ formatUsageUser(log) }}</div>
                    <div class="mt-0.5 text-xs text-gray-500">{{ log.user?.email || `#${log.user_id}` }}</div>
                  </td>
                  <td class="px-4 py-3">{{ log.model || '-' }}</td>
                  <td class="px-4 py-3">{{ log.group?.name || (log.group_id ? `#${log.group_id}` : '-') }}</td>
                  <td class="px-4 py-3 text-right">{{ formatNumber(totalUsageTokens(log)) }}</td>
                  <td class="px-4 py-3 text-right">{{ formatCurrency(log.actual_cost) }}</td>
                  <td class="px-4 py-3">{{ formatDateTime(log.created_at) }}</td>
                </tr>
                <tr v-if="!usageLogsLoading && usageLogs.length === 0">
                  <td colspan="6" class="px-4 py-8 text-center text-gray-500">暂无使用记录</td>
                </tr>
              </tbody>
            </table>
          </div>
          <Pagination
            v-if="usagePagination.total > 0"
            :page="usagePagination.page"
            :total="usagePagination.total"
            :page-size="usagePagination.page_size"
            @update:page="handleUsagePageChange"
            @update:pageSize="handleUsagePageSizeChange"
          />
        </div>
      </section>
    </div>

    <div v-if="editor.visible && editor.user" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 px-4">
      <div class="w-full max-w-xl rounded-lg bg-white shadow-xl dark:bg-dark-800">
        <div class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            分配分组：{{ editor.user.name || editor.user.local_email || editor.user.open_id }}
          </h2>
          <button class="text-gray-400 hover:text-gray-600" @click="closeEditor">×</button>
        </div>
        <div class="max-h-[60vh] overflow-y-auto px-5 py-4">
          <div class="grid gap-2">
            <label v-for="group in editor.user.assignable_groups || []" :key="group.id" class="flex items-center gap-2 rounded border border-gray-200 px-3 py-2 text-sm dark:border-dark-700">
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
import { computed, onMounted, reactive, ref } from 'vue'
import Pagination from '@/components/common/Pagination.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import feishuOrgAPI, { type FeishuOrgGroupBrief, type FeishuOrgUser } from '@/api/admin/feishuOrg'
import { useAppStore } from '@/stores'
import type { GroupStat, ModelStat, UsageLog, UsageStatsResponse } from '@/types'
import { formatCurrency, formatDateTime, formatNumber } from '@/utils/format'

const appStore = useAppStore()
type OrgManagerTab = 'members' | 'summary' | 'records'
type UsageRange = '1d' | '7d' | '30d'

const tabs: Array<{ value: OrgManagerTab; label: string }> = [
  { value: 'members', label: '成员授权' },
  { value: 'summary', label: '部门统计' },
  { value: 'records', label: '使用明细' }
]

const activeTab = ref<OrgManagerTab>('members')
const loading = ref(false)
const saving = ref(false)
const users = ref<FeishuOrgUser[]>([])
const pagination = reactive({ page: 1, page_size: 20, total: 0 })
const selectedGroupIds = ref<number[]>([])
const search = ref('')
const searchDraft = ref('')
const editor = reactive<{ visible: boolean; user?: FeishuOrgUser }>({ visible: false })
const usageRange = ref<UsageRange>('7d')
const usageLoading = ref(false)
const usageLogsLoading = ref(false)
const usageStats = ref<UsageStatsResponse | null>(null)
const modelStats = ref<ModelStat[]>([])
const groupStats = ref<GroupStat[]>([])
const usageLogs = ref<UsageLog[]>([])
const usagePagination = reactive({ page: 1, page_size: 20, total: 0 })
const currentLoading = computed(() => {
  if (activeTab.value === 'members') return loading.value
  if (activeTab.value === 'summary') return usageLoading.value
  return usageLogsLoading.value
})

onMounted(() => {
  void loadUsers()
})

async function loadUsers() {
  loading.value = true
  try {
    const result = await feishuOrgAPI.listManagedUsers({
      q: normalizeSearchParam(search.value),
      limit: pagination.page_size,
      offset: (pagination.page - 1) * pagination.page_size
    })
    users.value = result.items || []
    pagination.total = typeof result.total === 'number' ? result.total : users.value.length
  } catch (error: any) {
    appStore.showError(error?.message || '加载可管理员工失败')
  } finally {
    loading.value = false
  }
}

function switchTab(tab: OrgManagerTab) {
  activeTab.value = tab
  if (tab === 'summary' && !usageStats.value) {
    void loadManagerUsageSummary()
  }
  if (tab === 'records' && usageLogs.value.length === 0) {
    void loadManagerUsageLogs()
  }
}

function refreshCurrentTab() {
  if (activeTab.value === 'members') {
    void loadUsers()
    return
  }
  if (activeTab.value === 'summary') {
    void loadManagerUsageSummary()
    return
  }
  void reloadManagerUsageLogs()
}

function openEditor(user: FeishuOrgUser) {
  editor.visible = true
  editor.user = user
  selectedGroupIds.value = [...user.department_manager_group_ids]
}

function closeEditor() {
  editor.visible = false
  editor.user = undefined
  selectedGroupIds.value = []
}

function applySearch() {
  search.value = searchDraft.value.trim()
  pagination.page = 1
  void loadUsers()
}

function clearSearch() {
  searchDraft.value = ''
  if (!search.value) return
  search.value = ''
  pagination.page = 1
  void loadUsers()
}

async function saveEditor() {
  if (!editor.user) return
  saving.value = true
  try {
    await feishuOrgAPI.setManagedUserGroupGrants(editor.user.user_id, {
      group_ids: selectedGroupIds.value,
      reason: 'manager_ui'
    })
    appStore.showSuccess('保存成功')
    closeEditor()
    await loadUsers()
  } catch (error: any) {
    appStore.showError(error?.message || '保存失败')
  } finally {
    saving.value = false
  }
}

async function loadManagerUsageSummary() {
  usageLoading.value = true
  try {
    const params = usageDateParams()
    const [stats, snapshot] = await Promise.all([
      feishuOrgAPI.getManagerUsageStats(params),
      feishuOrgAPI.getManagerUsageSnapshotV2({
        ...params,
        include_trend: false,
        include_model_stats: true,
        include_group_stats: true
      })
    ])
    usageStats.value = stats
    modelStats.value = snapshot.models || []
    groupStats.value = snapshot.groups || []
  } catch (error: any) {
    appStore.showError(error?.message || '加载部门统计失败')
  } finally {
    usageLoading.value = false
  }
}

async function loadManagerUsageLogs() {
  usageLogsLoading.value = true
  try {
    const result = await feishuOrgAPI.queryManagerUsage({
      ...usageDateParams(),
      page: usagePagination.page,
      page_size: usagePagination.page_size,
      sort_by: 'created_at',
      sort_order: 'desc'
    })
    usageLogs.value = result.items || []
    usagePagination.total = typeof result.total === 'number' ? result.total : usageLogs.value.length
  } catch (error: any) {
    appStore.showError(error?.message || '加载使用明细失败')
  } finally {
    usageLogsLoading.value = false
  }
}

function reloadManagerUsageLogs() {
  usagePagination.page = 1
  void loadManagerUsageLogs()
}

function formatGroupIds(ids: number[], groups: FeishuOrgGroupBrief[]) {
  if (!ids || ids.length === 0) return '-'
  return ids.map((id) => groups.find((group) => group.id === id)?.name || `#${id}`).join('、')
}

function formatDuration(value: number | null | undefined) {
  if (!value) return '0ms'
  if (value >= 1000) return `${(value / 1000).toFixed(2)}s`
  return `${Math.round(value)}ms`
}

function formatUsageUser(log: UsageLog) {
  return log.user?.username || log.user?.email || `#${log.user_id}`
}

function totalUsageTokens(log: UsageLog) {
  return (log.input_tokens || 0) + (log.output_tokens || 0) + (log.cache_creation_tokens || 0) + (log.cache_read_tokens || 0)
}

function handlePageChange(page: number) {
  pagination.page = page
  void loadUsers()
}

function handlePageSizeChange(pageSize: number) {
  pagination.page_size = pageSize
  pagination.page = 1
  void loadUsers()
}

function handleUsagePageChange(page: number) {
  usagePagination.page = page
  void loadManagerUsageLogs()
}

function handleUsagePageSizeChange(pageSize: number) {
  usagePagination.page_size = pageSize
  usagePagination.page = 1
  void loadManagerUsageLogs()
}

function normalizeSearchParam(value: string) {
  const trimmed = value.trim()
  return trimmed || undefined
}

function usageDateParams() {
  const end = new Date()
  const start = new Date(end)
  if (usageRange.value === '7d') {
    start.setDate(start.getDate() - 6)
  } else if (usageRange.value === '30d') {
    start.setDate(start.getDate() - 29)
  }
  return {
    start_date: formatISODate(start),
    end_date: formatISODate(end),
    timezone: getClientTimeZone()
  }
}

function formatISODate(date: Date) {
  return date.toISOString().slice(0, 10)
}

function getClientTimeZone() {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || undefined
  } catch {
    return undefined
  }
}
</script>
