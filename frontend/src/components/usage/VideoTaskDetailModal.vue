<template>
  <BaseDialog :show="show" :title="t('videoTasks.detail.title')" width="wide" @close="close">
    <div v-if="loading" class="flex justify-center py-12">
      <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
    </div>
    <div v-else-if="detail" class="space-y-6">
      <div class="grid grid-cols-2 gap-4 rounded-xl bg-gray-50 p-4 text-sm dark:bg-dark-800 md:grid-cols-4">
        <InfoItem :label="t('videoTasks.columns.requestId')" :value="detail.task.request_id" mono />
        <InfoItem :label="t('videoTasks.columns.user')" :value="`#${detail.task.user_id}`" />
        <InfoItem :label="t('videoTasks.filters.group')" :value="`#${detail.task.group_id}`" />
        <InfoItem :label="t('videoTasks.columns.routing')" :value="`${detail.task.provider} · ${detail.task.external_model}`" />
        <InfoItem :label="t('videoTasks.filters.operation')" :value="detail.task.operation" />
        <InfoItem :label="t('videoTasks.columns.status')" :value="t(`videoTasks.status.${detail.task.status}`)" />
        <InfoItem :label="t('videoTasks.detail.billingStatus')" :value="detail.task.billing_status" />
        <InfoItem :label="t('videoTasks.columns.frozen')" :value="money(detail.task.frozen_amount)" />
        <InfoItem :label="t('videoTasks.columns.final')" :value="money(detail.task.settled_amount)" />
        <InfoItem :label="t('videoTasks.columns.upstream')" :value="money(detail.task.actual_upstream_cost)" />
      </div>

      <section data-testid="video-task-timestamps" class="space-y-2">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('videoTasks.detail.lifecycle') }}</h4>
        <div class="grid grid-cols-1 gap-3 rounded-xl border border-gray-200 p-4 text-sm dark:border-dark-700 md:grid-cols-3">
          <InfoItem :label="t('videoTasks.detail.createdAt')" :value="formatOptionalDate(detail.task.created_at)" />
          <InfoItem :label="t('videoTasks.detail.updatedAt')" :value="formatOptionalDate(detail.task.updated_at)" />
          <InfoItem :label="t('videoTasks.detail.submittedAt')" :value="formatOptionalDate(detail.task.submitted_at)" />
          <InfoItem :label="t('videoTasks.detail.startedAt')" :value="formatOptionalDate(detail.task.started_at)" />
          <InfoItem :label="t('videoTasks.detail.finishedAt')" :value="formatOptionalDate(detail.task.finished_at)" />
          <InfoItem :label="t('videoTasks.detail.settledAt')" :value="formatOptionalDate(detail.task.settled_at)" />
        </div>
      </section>

      <section v-if="resultHost || resultExpiry" class="space-y-2">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('videoTasks.detail.result') }}</h4>
        <div class="grid grid-cols-1 gap-3 rounded-xl border border-gray-200 p-4 text-sm dark:border-dark-700 md:grid-cols-2">
          <InfoItem :label="t('videoTasks.detail.resultHost')" :value="resultHost || '-'" />
          <InfoItem :label="t('videoTasks.detail.resultExpiry')" :value="resultExpiry ? formatDateTime(resultExpiry) : '-'" />
        </div>
      </section>

      <section v-if="detail.task.last_error_code" class="rounded-xl border border-red-200 bg-red-50 p-4 dark:border-red-900/50 dark:bg-red-900/10">
        <h4 class="text-sm font-semibold text-red-800 dark:text-red-300">{{ t('videoTasks.detail.normalizedError') }}</h4>
        <p class="mt-1 font-mono text-xs text-red-700 dark:text-red-300">{{ detail.task.last_error_code }}</p>
      </section>

      <section class="space-y-3">
        <h4 class="text-sm font-semibold text-gray-900 dark:text-gray-100">{{ t('videoTasks.detail.timeline') }}</h4>
        <div v-if="detail.events.length === 0" class="rounded-xl border border-dashed border-gray-200 p-6 text-center text-sm text-gray-500 dark:border-dark-700">
          {{ t('videoTasks.detail.noEvents') }}
        </div>
        <ol v-else class="space-y-3">
          <li v-for="event in safeEvents" :key="event.id" class="flex gap-3">
            <div class="mt-1.5 h-2 w-2 shrink-0 rounded-full bg-primary-500"></div>
            <div class="min-w-0 flex-1 rounded-xl border border-gray-200 p-3 dark:border-dark-700">
              <div class="flex flex-wrap items-center justify-between gap-2">
                <span class="font-medium text-gray-900 dark:text-gray-100">{{ event.event_type }}</span>
                <span class="text-xs text-gray-500">{{ formatDateTime(event.created_at) }}</span>
              </div>
              <dl v-if="event.fields.length" class="mt-2 grid grid-cols-1 gap-1 text-xs md:grid-cols-2">
                <div v-for="field in event.fields" :key="field.label" class="flex gap-2">
                  <dt class="text-gray-500">{{ field.label }}:</dt>
                  <dd class="break-words text-gray-700 dark:text-gray-300">{{ field.value }}</dd>
                </div>
              </dl>
            </div>
          </li>
        </ol>
      </section>
    </div>

    <template #footer>
      <div v-if="detail" class="flex flex-wrap justify-end gap-2">
        <button
          type="button"
          class="btn btn-secondary"
          data-testid="video-task-reconcile"
          :disabled="actionBusy || terminal"
          @click="openAction('reconcile')"
        >{{ t('videoTasks.actions.reconcile') }}</button>
        <button
          type="button"
          class="btn btn-danger"
          data-testid="video-task-refund"
          :disabled="actionBusy || !canFinancialAction"
          @click="openAction('refund')"
        >{{ t('videoTasks.actions.refund') }}</button>
        <button
          type="button"
          class="btn btn-primary"
          data-testid="video-task-complete"
          :disabled="actionBusy || !canFinancialAction"
          @click="openAction('complete')"
        >{{ t('videoTasks.actions.complete') }}</button>
      </div>
    </template>
  </BaseDialog>

  <BaseDialog
    :show="pendingAction !== null"
    :title="pendingAction ? t(`videoTasks.actions.confirm.${pendingAction}`) : ''"
    width="narrow"
    :close-on-escape="!actionBusy"
    @close="closeAction"
  >
    <div class="space-y-4">
      <p class="text-sm text-gray-600 dark:text-gray-400">{{ t('videoTasks.actions.confirmHint') }}</p>
      <TextArea v-model="actionReason" :label="t('videoTasks.actions.reason')" :rows="3" :disabled="actionLocked" required />
      <template v-if="pendingAction === 'complete'">
        <Input v-model="completeForm.provider_task_id" :label="t('videoTasks.actions.providerTaskId')" :disabled="actionLocked" required />
        <Input v-model="completeForm.result_url" :label="t('videoTasks.actions.resultUrl')" type="url" :disabled="actionLocked" required />
        <div class="grid grid-cols-2 gap-3">
          <Input v-model="completeForm.duration_seconds" :label="t('videoTasks.actions.duration')" type="number" :disabled="actionLocked" required />
          <Select v-model="completeForm.resolution" :options="resolutionOptions" :aria-label="t('videoTasks.actions.resolution')" :disabled="actionLocked" />
        </div>
        <Input v-model="completeForm.final_amount" :label="t('videoTasks.actions.finalAmount')" type="number" :disabled="actionLocked" required />
      </template>
      <p v-if="actionError" class="text-sm text-red-600 dark:text-red-400">{{ actionError }}</p>
    </div>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="actionBusy" @click="closeAction">{{ t('common.cancel') }}</button>
        <button
          type="button"
          class="btn btn-primary"
          data-testid="video-task-confirm"
          :disabled="actionBusy || !actionValid"
          @click="confirmAction"
        >{{ actionBusy ? t('common.processing') : t('common.confirm') }}</button>
      </div>
    </template>
  </BaseDialog>
  <TotpStepUpDialog :controller="stepUp" />
</template>

<script setup lang="ts">
import { computed, defineComponent, h, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Input from '@/components/common/Input.vue'
import Select from '@/components/common/Select.vue'
import TextArea from '@/components/common/TextArea.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { adminUsageAPI } from '@/api/admin/usage'
import type { AdminVideoTaskDetail, CompleteVideoTaskRequest } from '@/api/admin/usage'
import { useAppStore } from '@/stores/app'
import { isStepUpCancelled, useStepUp } from '@/composables/useStepUp'
import { formatCostFixed, formatDateTime } from '@/utils/format'

const InfoItem = defineComponent({
  props: { label: { type: String, required: true }, value: { type: String, required: true }, mono: Boolean },
  setup(props) {
    return () => h('div', { class: 'min-w-0' }, [
      h('div', { class: 'text-xs text-gray-500 dark:text-gray-400' }, props.label),
      h('div', { class: ['mt-1 break-words text-gray-900 dark:text-gray-100', props.mono ? 'font-mono text-xs' : ''] }, props.value),
    ])
  },
})

const props = defineProps<{ show: boolean; detail: AdminVideoTaskDetail | null; loading: boolean }>()
const emit = defineEmits<{ close: []; refreshed: [requestId: string] }>()
const { t } = useI18n()
const appStore = useAppStore()
const stepUp = useStepUp()
type Action = 'reconcile' | 'refund' | 'complete'
const pendingAction = ref<Action | null>(null)
const actionReason = ref('')
const actionBusy = ref(false)
const actionError = ref('')
const actionKey = ref('')
interface ActionSnapshot {
  action: Action
  requestId: string
  idempotencyKey: string
  reason: string
  completion: CompleteVideoTaskRequest
}
const actionSnapshot = ref<ActionSnapshot | null>(null)
let actionGeneration = 0
const completeForm = reactive({ provider_task_id: '', result_url: '', duration_seconds: '', resolution: '720p', final_amount: '' })
const resolutionOptions = [
  { value: '480p', label: '480p' }, { value: '720p', label: '720p' }, { value: '1080p', label: '1080p' },
]

const terminal = computed(() => ['succeeded', 'failed', 'cancelled'].includes(props.detail?.task.status ?? ''))
const canFinancialAction = computed(() => {
  const task = props.detail?.task
  return !!task && task.billing_status === 'held' && !task.settled_at && ['unknown', 'failed'].includes(task.status)
})
const resultSummary = computed(() => props.detail?.result_url_summary || props.detail?.task.result_url_summary || '')
const resultHost = computed(() => {
  try { return new URL(resultSummary.value).host }
  catch { return '' }
})
const resultExpiry = computed(() => props.detail?.task.result_url_expires_at ?? null)

const sanitizeEventText = (raw: string) => {
  let text = raw.slice(0, 512)
  text = text.replace(/\bBearer\s+\S+/gi, 'Bearer [redacted]')
  text = text.replace(
    /\b(api[_-]?key|authorization|access[_-]?token|refresh[_-]?token|secret|signature)\b\s*[:=]\s*\S+/gi,
    '$1=[redacted]',
  )
  return text.replace(/https?:\/\/[^\s]+/gi, (candidate) => {
    try {
      const url = new URL(candidate)
      return `${url.protocol}//${url.host}${url.pathname}`
    } catch {
      return '[redacted-url]'
    }
  })
}

const safeEvents = computed(() => (props.detail?.events ?? []).map((event) => {
  const payload = typeof event.payload === 'object' && event.payload !== null && !Array.isArray(event.payload)
    ? event.payload as Record<string, unknown>
    : {}
  const allowed = ['status', 'billing_status', 'error_code', 'message', 'progress', 'provider_status']
  return {
    id: event.id,
    event_type: String(event.event_type).slice(0, 128),
    created_at: event.created_at,
    fields: allowed.flatMap((key) => {
      const value = payload[key]
      return typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean'
        ? [{ label: key, value: sanitizeEventText(String(value)) }]
        : []
    }),
  }
}))

const actionValid = computed(() => {
  if (actionSnapshot.value) return true
  if (!actionReason.value.trim()) return false
  if (pendingAction.value !== 'complete') return true
  const duration = Number(completeForm.duration_seconds)
  const amount = Number(completeForm.final_amount)
  return !!completeForm.provider_task_id.trim() && !!completeForm.result_url.trim()
    && Number.isFinite(duration) && duration > 0 && Number.isFinite(amount) && amount >= 0
})
const actionLocked = computed(() => actionSnapshot.value !== null)

const newKey = (action: Action, requestId: string) => {
  const uuid = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
  return `video-${action}-${requestId}-${uuid}`
}
const openAction = (action: Action) => {
  if (!props.detail || (action !== 'reconcile' && !canFinancialAction.value) || (action === 'reconcile' && terminal.value)) return
  pendingAction.value = action
  actionReason.value = ''
  actionError.value = ''
  actionKey.value = newKey(action, props.detail.task.request_id)
  actionSnapshot.value = null
  actionGeneration++
  Object.assign(completeForm, { provider_task_id: '', result_url: '', duration_seconds: '', resolution: '720p', final_amount: '' })
}
const resetAction = () => {
  pendingAction.value = null
  actionReason.value = ''
  actionError.value = ''
  actionKey.value = ''
  actionSnapshot.value = null
}
const closeAction = () => {
  if (actionBusy.value) return
  actionGeneration++
  resetAction()
}
const close = () => {
  if (actionBusy.value) return
  closeAction()
  emit('close')
}

const confirmAction = async () => {
  const action = pendingAction.value
  const task = props.detail?.task
  if (!action || !task || !actionValid.value || !actionKey.value) return
  const generation = actionGeneration
  const snapshot = actionSnapshot.value ?? {
    action,
    requestId: task.request_id,
    idempotencyKey: actionKey.value,
    reason: actionReason.value.trim(),
    completion: {
      reason: actionReason.value.trim(),
      provider_task_id: completeForm.provider_task_id.trim(),
      result_url: completeForm.result_url.trim(),
      duration_seconds: Number(completeForm.duration_seconds),
      resolution: completeForm.resolution as CompleteVideoTaskRequest['resolution'],
      final_amount: Number(completeForm.final_amount),
    },
  }
  actionSnapshot.value = snapshot
  actionBusy.value = true
  actionError.value = ''
  try {
    await stepUp.run(() => {
      if (snapshot.action === 'reconcile') return adminUsageAPI.reconcileVideoTask(snapshot.requestId, snapshot.reason, snapshot.idempotencyKey)
      if (snapshot.action === 'refund') return adminUsageAPI.refundVideoTask(snapshot.requestId, snapshot.reason, snapshot.idempotencyKey)
      return adminUsageAPI.completeVideoTask(snapshot.requestId, snapshot.completion, snapshot.idempotencyKey)
    })
    if (generation === actionGeneration && props.detail?.task.request_id === snapshot.requestId) {
      resetAction()
      appStore.showSuccess(t('videoTasks.actions.success'))
      emit('refreshed', snapshot.requestId)
    }
  } catch (error: unknown) {
    if (isStepUpCancelled(error) || generation !== actionGeneration) return
    const message = typeof error === 'object' && error !== null && 'message' in error
      ? String((error as { message?: unknown }).message ?? '')
      : ''
    actionError.value = message || t('videoTasks.actions.failed')
    appStore.showError(actionError.value)
  } finally {
    actionBusy.value = false
  }
}

watch(() => props.detail?.task.request_id, () => {
  actionGeneration++
  resetAction()
})
watch(() => props.show, (show) => {
  if (!show) {
    actionGeneration++
    resetAction()
  }
})

const money = (value: number | null | undefined) => value == null ? '-' : `$${formatCostFixed(value, 4)}`
const formatOptionalDate = (value: string | null | undefined) => value ? formatDateTime(value) : '-'

defineExpose({ actionReason })
</script>
