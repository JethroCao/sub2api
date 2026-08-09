<template>
  <section class="space-y-3" data-testid="video-pricing-editor">
    <div class="flex items-start justify-between gap-3">
      <div>
        <h4 class="text-sm font-medium text-gray-700 dark:text-gray-300">
          {{ t('admin.groups.videoPricing.rulesTitle') }}
        </h4>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.groups.videoPricing.rulesHint') }}
        </p>
      </div>
      <button
        type="button"
        class="btn btn-secondary flex-shrink-0"
        :disabled="modelOptions.length === 0"
        data-testid="video-pricing-add"
        @click="addRule"
      >
        {{ t('admin.groups.videoPricing.addRule') }}
      </button>
    </div>

    <div
      v-if="missingCoverage.length > 0"
      data-testid="video-pricing-missing"
      class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-800 dark:border-amber-800/60 dark:bg-amber-900/20 dark:text-amber-300"
    >
      <div class="font-medium">{{ t('admin.groups.videoPricing.missingCoverageTitle') }}</div>
      <ul class="mt-1 list-disc space-y-1 pl-4">
        <li v-for="item in missingCoverage" :key="`${item.external_model}:${item.operation}`">
          {{ t('admin.groups.videoPricing.missingCoverageItem', item) }}
        </li>
      </ul>
    </div>

    <p
      v-if="draft.length === 0"
      class="rounded-lg border border-dashed border-gray-300 p-4 text-center text-xs text-gray-500 dark:border-dark-600 dark:text-gray-400"
    >
      {{ t(modelOptions.length > 0 ? 'admin.groups.videoPricing.empty' : 'admin.groups.videoPricing.noAuthoritativeModels') }}
    </p>

    <div
      v-for="(row, index) in draft"
      :key="rowKeys[index]"
      class="rounded-lg border border-gray-200 bg-gray-50/50 p-3 dark:border-dark-600 dark:bg-dark-800/40"
    >
      <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-4">
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.externalModel') }}</label>
          <select
            v-model="row.external_model"
            class="input"
            :data-testid="`video-pricing-model-${index}`"
            @change="emitDraft"
          >
            <option v-for="model in rowModelOptions(row.external_model)" :key="model" :value="model">
              {{ model }}
            </option>
          </select>
        </div>
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.operation') }}</label>
          <select
            v-model="row.operation"
            class="input"
            :data-testid="`video-pricing-operation-${index}`"
            @change="emitDraft"
          >
            <option value="generation">{{ t('admin.groups.videoPricing.operations.generation') }}</option>
            <option value="edit">{{ t('admin.groups.videoPricing.operations.edit') }}</option>
            <option value="extension">{{ t('admin.groups.videoPricing.operations.extension') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.resolution') }}</label>
          <select
            v-model="row.resolution"
            class="input"
            :data-testid="`video-pricing-resolution-${index}`"
            @change="emitDraft"
          >
            <option value="*">{{ t('admin.groups.videoPricing.anyResolution') }}</option>
            <option value="480p">480p</option>
            <option value="720p">720p</option>
            <option value="1080p">1080p</option>
          </select>
        </div>
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.audioMode') }}</label>
          <select
            v-model="row.audio_mode"
            class="input"
            :data-testid="`video-pricing-audio-${index}`"
            @change="emitDraft"
          >
            <option value="any">{{ t('admin.groups.videoPricing.audioModes.any') }}</option>
            <option value="with_audio">{{ t('admin.groups.videoPricing.audioModes.withAudio') }}</option>
            <option value="without_audio">{{ t('admin.groups.videoPricing.audioModes.withoutAudio') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.unit') }}</label>
          <select
            v-model="row.unit"
            class="input"
            :data-testid="`video-pricing-unit-${index}`"
            @change="emitDraft"
          >
            <option value="per_request">{{ t('admin.groups.videoPricing.units.perRequest') }}</option>
            <option value="per_output_second">{{ t('admin.groups.videoPricing.units.perOutputSecond') }}</option>
          </select>
        </div>
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.customerUnitPrice') }}</label>
          <input
            :value="row.unit_price"
            type="number"
            min="0"
            step="0.0000000001"
            class="input"
            :data-testid="`video-pricing-unit-price-${index}`"
            @input="setNumber(index, 'unit_price', $event)"
          />
        </div>
        <div>
          <label class="input-label">{{ t('admin.groups.videoPricing.upstreamUnitCost') }}</label>
          <input
            :value="row.upstream_unit_cost ?? ''"
            type="number"
            min="0"
            step="0.0000000001"
            class="input"
            :placeholder="t('admin.groups.videoPricing.unknownCost')"
            :data-testid="`video-pricing-upstream-cost-${index}`"
            @input="setNullableNumber(index, $event)"
          />
        </div>
        <div class="flex items-end justify-between gap-3 pb-2">
          <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
            <input
              v-model="row.enabled"
              type="checkbox"
              class="rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :data-testid="`video-pricing-enabled-${index}`"
              @change="emitDraft"
            />
            {{ t('admin.groups.videoPricing.enabled') }}
          </label>
          <button
            type="button"
            class="text-xs font-medium text-red-600 hover:text-red-700 dark:text-red-400"
            :aria-label="t('admin.groups.videoPricing.removeRule')"
            @click="removeRule(index)"
          >
            {{ t('common.delete') }}
          </button>
        </div>
      </div>

      <p
        v-if="rowErrorIndexes.has(index)"
        class="mt-2 text-xs text-red-600 dark:text-red-400"
        :data-testid="`video-pricing-error-${index}`"
      >
        {{ t('admin.groups.videoPricing.errors.row') }}
      </p>
      <div
        v-if="row.unit === 'per_output_second' && Number.isFinite(row.unit_price) && row.unit_price >= 0"
        class="mt-3 rounded bg-white p-2 text-xs text-gray-600 dark:bg-dark-800 dark:text-gray-300"
        :data-testid="`video-pricing-preview-${index}`"
      >
        {{ t('admin.groups.videoPricing.customerPreview', {
          five: formatMoney(preview(row).five_second_price),
          ten: formatMoney(preview(row).ten_second_price)
        }) }}
        <div
          v-if="row.upstream_unit_cost !== null"
          class="mt-1 text-gray-500 dark:text-gray-400"
          :data-testid="`video-pricing-margin-${index}`"
        >
          {{ t('admin.groups.videoPricing.marginPreview', {
            five: formatMoney(preview(row).five_second_margin),
            ten: formatMoney(preview(row).ten_second_margin)
          }) }}
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import type { VideoPricingCapability, VideoPricingRuleInput } from '@/types'
import {
  buildVideoPricingPreview,
  validateVideoPricingRules,
  videoPricingRulesForReplacement
} from '@/views/admin/groupsVideoPricing'

const props = defineProps<{
  modelValue: VideoPricingRuleInput[]
  capabilities: VideoPricingCapability[]
}>()

const emit = defineEmits<{
  'update:modelValue': [rules: VideoPricingRuleInput[]]
}>()

const { t } = useI18n()
let nextRowKey = 1
const rowKeys = ref<number[]>([])
const draft = ref<VideoPricingRuleInput[]>([])

const syncDraft = (rules: VideoPricingRuleInput[]) => {
  draft.value = videoPricingRulesForReplacement(rules)
  rowKeys.value = draft.value.map(() => nextRowKey++)
}
syncDraft(props.modelValue)
watch(() => props.modelValue, (rules) => {
  const normalized = videoPricingRulesForReplacement(rules)
  if (JSON.stringify(normalized) !== JSON.stringify(draft.value)) syncDraft(normalized)
}, { deep: true })

const validationErrors = computed(() => validateVideoPricingRules(draft.value, props.capabilities))
const missingCoverage = computed(() => validationErrors.value.filter(
  (error): error is Extract<(typeof validationErrors.value)[number], { code: 'coverage' }> => error.code === 'coverage'
))
const rowErrorIndexes = computed(() => new Set(validationErrors.value.flatMap(error =>
  'row' in error ? [error.row] : [])))
const modelOptions = computed(() => Array.from(new Set(
  props.capabilities.map(capability => capability.external_model)
)).sort())

const rowModelOptions = (current: string) => Array.from(new Set(
  [...modelOptions.value, current].filter(Boolean)
)).sort()

const emitDraft = () => emit('update:modelValue', videoPricingRulesForReplacement(draft.value))
const addRule = () => {
  const capability = props.capabilities[0]
  if (!capability) return
  draft.value.push({
    external_model: capability.external_model,
    operation: capability.operation,
    resolution: '*',
    audio_mode: 'any',
    unit: 'per_output_second',
    unit_price: 0,
    upstream_unit_cost: null,
    enabled: true
  })
  rowKeys.value.push(nextRowKey++)
  emitDraft()
}
const removeRule = (index: number) => {
  draft.value.splice(index, 1)
  rowKeys.value.splice(index, 1)
  emitDraft()
}
const setNumber = (index: number, field: 'unit_price', event: Event) => {
  draft.value[index][field] = Number((event.target as HTMLInputElement).value)
  emitDraft()
}
const setNullableNumber = (index: number, event: Event) => {
  const value = (event.target as HTMLInputElement).value
  draft.value[index].upstream_unit_cost = value.trim() === '' ? null : Number(value)
  emitDraft()
}
const preview = (rule: VideoPricingRuleInput) => buildVideoPricingPreview(rule)
const formatMoney = (value: number | null) => {
  if (value === null || !Number.isFinite(value)) return ''
  return `$${value.toFixed(10).replace(/0+$/, '').replace(/\.$/, '')}`
}
const validate = () => validationErrors.value.length === 0

defineExpose({ validate })
</script>
