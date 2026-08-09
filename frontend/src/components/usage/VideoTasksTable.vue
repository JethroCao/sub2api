<template>
  <div class="flex min-h-0 flex-col">
    <div class="border-b border-gray-100 p-4 dark:border-dark-700/50">
      <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
        <Select v-model="draftFilters.provider" :options="providerOptions" :aria-label="t('videoTasks.filters.provider')" />
        <Input v-model="draftFilters.model" :placeholder="t('videoTasks.filters.model')" />
        <Select v-model="draftFilters.operation" :options="operationOptions" :aria-label="t('videoTasks.filters.operation')" />
        <Select v-model="draftFilters.status" :options="statusOptions" :aria-label="t('videoTasks.filters.status')" />
        <Input v-model="draftFilters.group_id" type="number" :placeholder="t('videoTasks.filters.group')" />
        <Input v-model="draftFilters.user_id" type="number" :placeholder="t('videoTasks.filters.user')" />
        <div class="md:col-span-2">
          <DateRangePicker
            v-model:start-date="draftFilters.start_date"
            v-model:end-date="draftFilters.end_date"
            @change="onDateChange"
          />
        </div>
      </div>
      <div class="mt-3 flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" data-testid="video-task-reset" @click="resetFilters">
          {{ t('common.reset') }}
        </button>
        <button type="button" class="btn btn-primary" data-testid="video-task-filter" @click="applyFilters">
          <Icon name="search" size="sm" />
          {{ t('common.search') }}
        </button>
      </div>
    </div>

    <DataTable
      :data="tasks"
      :columns="columns"
      :loading="loading"
      row-key="request_id"
      clickable-rows
      @row-click="emit('open', $event)"
    >
      <template #cell-request_id="{ row }">
        <span class="font-mono text-xs text-primary-600 dark:text-primary-400">{{ row.request_id }}</span>
      </template>
      <template #cell-user_id="{ row }">
        <span class="tabular-nums">#{{ row.user_id }}</span>
      </template>
      <template #cell-routing="{ row }">
        <div class="space-y-0.5">
          <div class="font-medium">{{ row.external_model }}</div>
          <div class="text-xs text-gray-500 dark:text-gray-400">{{ row.provider }} · {{ operationLabel(row.operation) }}</div>
        </div>
      </template>
      <template #cell-status="{ row }">
        <div class="space-y-1">
          <span :class="statusClass(row.status)" class="inline-flex rounded-full px-2 py-0.5 text-xs font-medium">
            {{ statusLabel(row.status) }}
          </span>
          <div v-if="row.upstream_status && row.upstream_status !== row.status" class="text-xs text-gray-500 dark:text-gray-400">
            {{ row.upstream_status }}
          </div>
        </div>
      </template>
      <template #cell-elapsed="{ row }">{{ elapsed(row) }}</template>
      <template #cell-cost="{ row }">
        <div class="space-y-0.5 text-xs tabular-nums">
          <div>{{ t('videoTasks.columns.frozen') }}: {{ money(row.frozen_amount) }}</div>
          <div>{{ t('videoTasks.columns.final') }}: {{ money(row.settled_amount) }}</div>
          <div>{{ t('videoTasks.columns.upstream') }}: {{ money(row.actual_upstream_cost) }}</div>
        </div>
      </template>
      <template #cell-created_at="{ value }">{{ formatDateTime(value) }}</template>
    </DataTable>

    <Pagination
      v-if="total > 0"
      :page="page"
      :total="total"
      :page-size="pageSize"
      @update:page="emit('update:page', $event)"
      @update:page-size="emit('update:pageSize', $event)"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, reactive } from 'vue'
import { useI18n } from 'vue-i18n'
import DataTable from '@/components/common/DataTable.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import Input from '@/components/common/Input.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatDateTime, formatCostFixed } from '@/utils/format'
import type { Column } from '@/components/common/types'
import type { AdminVideoTask } from '@/api/admin/usage'

export interface VideoTaskFilters {
  provider: string
  model: string
  operation: string
  status: string
  group_id: number | undefined
  user_id: number | undefined
  start_date: string
  end_date: string
}

defineProps<{
  tasks: AdminVideoTask[]
  total: number
  loading: boolean
  page: number
  pageSize: number
}>()

const emit = defineEmits<{
  open: [task: AdminVideoTask]
  'filter-change': [filters: VideoTaskFilters]
  'update:page': [page: number]
  'update:pageSize': [pageSize: number]
}>()

const { t } = useI18n()
const emptyFilters = (): Record<string, string> => ({
  provider: '', model: '', operation: '', status: '', group_id: '', user_id: '', start_date: '', end_date: '',
})
const draftFilters = reactive(emptyFilters())

const providerOptions = computed(() => [
  { value: '', label: t('videoTasks.filters.allProviders') },
  { value: 'grok', label: 'Grok' },
  { value: 'seedance', label: 'Seedance' },
  { value: 'kling', label: 'Kling' },
])
const operationOptions = computed(() => [
  { value: '', label: t('videoTasks.filters.allOperations') },
  ...(['generation', 'edit', 'extension'] as const).map((value) => ({ value, label: operationLabel(value) })),
])
const statusOptions = computed(() => [
  { value: '', label: t('videoTasks.filters.allStatuses') },
  ...(['created', 'submitting', 'submitted', 'queued', 'running', 'succeeded', 'failed', 'cancelled', 'unknown', 'manual_review'] as const)
    .map((value) => ({ value, label: statusLabel(value) })),
])

const columns = computed<Column[]>(() => [
  { key: 'request_id', label: t('videoTasks.columns.requestId') },
  { key: 'user_id', label: t('videoTasks.columns.user') },
  { key: 'routing', label: t('videoTasks.columns.routing') },
  { key: 'status', label: t('videoTasks.columns.status') },
  { key: 'elapsed', label: t('videoTasks.columns.elapsed') },
  { key: 'cost', label: t('videoTasks.columns.cost') },
  { key: 'created_at', label: t('videoTasks.columns.createdAt') },
])

const numeric = (value: string): number | undefined => {
  if (!value.trim()) return undefined
  const result = Number(value)
  return Number.isSafeInteger(result) && result > 0 ? result : undefined
}

const applyFilters = () => emit('filter-change', {
  provider: draftFilters.provider,
  model: draftFilters.model.trim(),
  operation: draftFilters.operation,
  status: draftFilters.status,
  group_id: numeric(draftFilters.group_id),
  user_id: numeric(draftFilters.user_id),
  start_date: draftFilters.start_date,
  end_date: draftFilters.end_date,
})

const resetFilters = () => {
  Object.assign(draftFilters, emptyFilters())
  applyFilters()
}
const onDateChange = (range: { startDate: string; endDate: string }) => {
  draftFilters.start_date = range.startDate
  draftFilters.end_date = range.endDate
}
const operationLabel = (value: string) => t(`videoTasks.operations.${value}`)
const statusLabel = (value: string) => t(`videoTasks.status.${value}`)
const statusClass = (value: string) => ({
  succeeded: 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300',
  failed: 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300',
  cancelled: 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300',
  unknown: 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300',
  manual_review: 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300',
}[value] ?? 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300')

const elapsed = (task: AdminVideoTask) => {
  const start = new Date(task.created_at).getTime()
  const end = task.finished_at ? new Date(task.finished_at).getTime() : Date.now()
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return '-'
  const seconds = Math.floor((end - start) / 1000)
  if (seconds < 60) return `${seconds}s`
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
  return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`
}
const money = (value: number | null | undefined) => value == null ? '-' : `$${formatCostFixed(value, 4)}`

defineExpose({ draftFilters, applyFilters })
</script>
