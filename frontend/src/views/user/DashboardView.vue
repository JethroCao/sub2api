<template>
  <AppLayout>
    <div class="space-y-6">
      <div v-if="loading" class="flex items-center justify-center py-12"><LoadingSpinner /></div>
      <template v-else-if="stats">
        <div v-if="canViewTeamUsage" class="card p-4">
          <div class="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h2 class="text-sm font-semibold text-gray-900 dark:text-white">统计范围</h2>
              <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">部门范围仅统计你可管理的已绑定员工用量。</p>
            </div>
            <div class="flex rounded-lg bg-gray-100 p-1 dark:bg-dark-700">
              <button class="rounded-md px-3 py-1.5 text-sm font-medium" :class="scopeButtonClass('personal')" @click="setUsageScope('personal')">个人</button>
              <button class="rounded-md px-3 py-1.5 text-sm font-medium" :class="scopeButtonClass('team')" @click="setUsageScope('team')">部门</button>
            </div>
          </div>
        </div>
        <UserDashboardStats :stats="stats" :balance="isTeamScope ? 0 : user?.balance || 0" :is-simple="authStore.isSimpleMode || isTeamScope" :hide-api-keys="isTeamScope" :platform-quotas="isTeamScope ? null : platformQuotas" />
        <UserDashboardCharts v-model:startDate="startDate" v-model:endDate="endDate" v-model:granularity="granularity" :loading="loadingCharts" :trend="trendData" :models="modelStats" @dateRangeChange="loadCharts" @granularityChange="loadCharts" @refresh="refreshAll" />
        <div class="grid grid-cols-1 gap-6 lg:grid-cols-3">
          <div class="lg:col-span-2"><UserDashboardRecentUsage :data="recentUsage" :loading="loadingUsage" /></div>
          <div class="lg:col-span-1"><UserDashboardQuickActions /></div>
        </div>
      </template>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'; import { useAuthStore } from '@/stores/auth'; import { usageAPI, type UserDashboardStats as UserStatsType } from '@/api/usage'
import AppLayout from '@/components/layout/AppLayout.vue'; import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import UserDashboardStats from '@/components/user/dashboard/UserDashboardStats.vue'; import UserDashboardCharts from '@/components/user/dashboard/UserDashboardCharts.vue'
import UserDashboardRecentUsage from '@/components/user/dashboard/UserDashboardRecentUsage.vue'; import UserDashboardQuickActions from '@/components/user/dashboard/UserDashboardQuickActions.vue'
import type { UsageLog, TrendDataPoint, ModelStat, PlatformQuotaItem } from '@/types'
import { getMyPlatformQuotas } from '@/api/user'
import feishuOrgAPI from '@/api/admin/feishuOrg'
import { formatDateLocalInput } from '@/utils/format'

const authStore = useAuthStore(); const user = computed(() => authStore.user)
const stats = ref<UserStatsType | null>(null); const loading = ref(false); const loadingUsage = ref(false); const loadingCharts = ref(false)
const trendData = ref<TrendDataPoint[]>([]); const modelStats = ref<ModelStat[]>([]); const recentUsage = ref<UsageLog[]>([])
const platformQuotas = ref<PlatformQuotaItem[] | null>(null)
type UsageScope = 'personal' | 'team'
const usageScope = ref<UsageScope>('personal')
const canViewTeamUsage = ref(false)
const isTeamScope = computed(() => canViewTeamUsage.value && usageScope.value === 'team')

const startDate = ref(formatDateLocalInput(new Date(Date.now() - 6 * 86400000))); const endDate = ref(formatDateLocalInput(new Date())); const granularity = ref('day')

const usageDateParams = () => ({ start_date: startDate.value, end_date: endDate.value, timezone: getClientTimeZone() })
const getClientTimeZone = () => { try { return Intl.DateTimeFormat().resolvedOptions().timeZone || undefined } catch { return undefined } }
const scopeButtonClass = (scope: UsageScope) => usageScope.value === scope ? 'bg-white text-primary-600 shadow-sm dark:bg-dark-800 dark:text-primary-300' : 'text-gray-600 hover:text-gray-900 dark:text-gray-300 dark:hover:text-white'
const setUsageScope = (scope: UsageScope) => { if (usageScope.value === scope) return; usageScope.value = scope; refreshAll() }

const detectTeamUsageAccess = async () => {
  try {
    const result = await feishuOrgAPI.getManagerAccess()
    canViewTeamUsage.value = result.has_access
  } catch {
    canViewTeamUsage.value = false
  }
}
const loadStats = async () => {
  loading.value = true
  try {
    if (isTeamScope.value) {
      stats.value = await feishuOrgAPI.getManagerDashboardStats({ timezone: getClientTimeZone() })
    } else {
      await authStore.refreshUser()
      stats.value = await usageAPI.getDashboardStats()
    }
  } catch (error) {
    console.error('Failed to load dashboard stats:', error)
  } finally {
    loading.value = false
  }
}
const loadCharts = async () => {
  loadingCharts.value = true
  try {
    if (isTeamScope.value) {
      const res = await Promise.all([
        feishuOrgAPI.getManagerUsageTrend({ ...usageDateParams(), granularity: granularity.value as any }),
        feishuOrgAPI.getManagerUsageModels(usageDateParams())
      ])
      trendData.value = res[0].trend || []
      modelStats.value = res[1].models || []
      return
    }
    const res = await Promise.all([usageAPI.getDashboardTrend({ start_date: startDate.value, end_date: endDate.value, granularity: granularity.value as any }), usageAPI.getDashboardModels({ start_date: startDate.value, end_date: endDate.value })])
    trendData.value = res[0].trend || []
    modelStats.value = res[1].models || []
  } catch (error) {
    console.error('Failed to load charts:', error)
  } finally {
    loadingCharts.value = false
  }
}
const loadRecent = async () => {
  loadingUsage.value = true
  try {
    if (isTeamScope.value) {
      const res = await feishuOrgAPI.queryManagerUsage({ ...usageDateParams(), page: 1, page_size: 5, sort_by: 'created_at', sort_order: 'desc' })
      recentUsage.value = res.items || []
      return
    }
    const res = await usageAPI.getByDateRange(startDate.value, endDate.value)
    recentUsage.value = res.items.slice(0, 5)
  } catch (error) {
    console.error('Failed to load recent usage:', error)
  } finally {
    loadingUsage.value = false
  }
}
const loadPlatformQuotas = async () => { if (isTeamScope.value) { platformQuotas.value = []; return } try { const data = await getMyPlatformQuotas(); platformQuotas.value = data.platform_quotas ?? [] } catch (error) { console.warn('Failed to load platform quotas:', error); platformQuotas.value = [] } }
const refreshAll = () => { loadStats(); loadCharts(); loadRecent(); loadPlatformQuotas() }

onMounted(async () => { await detectTeamUsageAccess(); refreshAll() })
</script>
