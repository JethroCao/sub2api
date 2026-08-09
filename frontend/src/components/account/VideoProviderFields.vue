<template>
  <section class="space-y-4" data-testid="video-provider-fields">
    <div>
      <label class="input-label">{{ t('admin.accounts.video.provider') }}</label>
      <Select
        :model-value="provider"
        :options="providerOptions"
        data-testid="video-provider"
        @update:model-value="handleProviderChange"
      />
    </div>

    <div v-if="provider === 'seedance'">
      <label class="input-label">{{ t('admin.accounts.video.apiKey') }}</label>
      <input
        :value="apiKey"
        type="password"
        class="input font-mono"
        autocomplete="new-password"
        data-1p-ignore
        data-lpignore="true"
        data-bwignore="true"
        data-testid="video-api-key"
        :placeholder="mode === 'edit' && credentialStatus.has_api_key ? t('admin.accounts.video.secretConfigured') : t('admin.accounts.video.apiKeyPlaceholder')"
        @input="handleCredentialInput('api_key', $event)"
      />
      <p v-if="mode === 'edit' && credentialStatus.has_api_key" class="input-hint">
        {{ t('admin.accounts.video.secretConfigured') }} · {{ t('admin.accounts.leaveEmptyToKeep') }}
      </p>
    </div>

    <template v-else-if="provider === 'kling'">
      <div>
        <label class="input-label">{{ t('admin.accounts.video.accessKey') }}</label>
        <input
          :value="accessKey"
          type="password"
          class="input font-mono"
          autocomplete="new-password"
          data-1p-ignore
          data-lpignore="true"
          data-bwignore="true"
          data-testid="video-access-key"
          :placeholder="mode === 'edit' && credentialStatus.has_access_key ? t('admin.accounts.video.secretConfigured') : t('admin.accounts.video.accessKeyPlaceholder')"
          @input="handleCredentialInput('access_key', $event)"
        />
        <p v-if="mode === 'edit' && credentialStatus.has_access_key" class="input-hint">
          {{ t('admin.accounts.video.secretConfigured') }} · {{ t('admin.accounts.leaveEmptyToKeep') }}
        </p>
      </div>
      <div>
        <label class="input-label">{{ t('admin.accounts.video.secretKey') }}</label>
        <input
          :value="secretKey"
          type="password"
          class="input font-mono"
          autocomplete="new-password"
          data-1p-ignore
          data-lpignore="true"
          data-bwignore="true"
          data-testid="video-secret-key"
          :placeholder="mode === 'edit' && credentialStatus.has_secret_key ? t('admin.accounts.video.secretConfigured') : t('admin.accounts.video.secretKeyPlaceholder')"
          @input="handleCredentialInput('secret_key', $event)"
        />
        <p v-if="mode === 'edit' && credentialStatus.has_secret_key" class="input-hint">
          {{ t('admin.accounts.video.secretConfigured') }} · {{ t('admin.accounts.leaveEmptyToKeep') }}
        </p>
      </div>
    </template>

    <div>
      <label class="input-label">{{ t('admin.accounts.baseUrl') }}</label>
      <input
        :value="baseUrl"
        type="url"
        class="input"
        data-testid="video-base-url"
        :placeholder="provider === 'seedance' ? 'https://ark.cn-beijing.volces.com' : t('admin.accounts.video.baseUrlPlaceholder')"
        @input="handleCredentialInput('base_url', $event)"
      />
      <p class="input-hint">{{ t('admin.accounts.video.baseUrlHint') }}</p>
    </div>

    <div class="border-t border-gray-200 pt-4 dark:border-dark-600">
      <label class="input-label">{{ t('admin.accounts.modelMapping') }}</label>
      <p class="mb-3 text-xs text-gray-500 dark:text-gray-400">
        {{ t('admin.accounts.video.modelMappingHint') }}
      </p>
      <div class="space-y-2">
        <div v-for="(mapping, index) in mappings" :key="mapping.id" class="flex items-center gap-2">
          <input
            v-model="mapping.from"
            type="text"
            class="input flex-1"
            :data-testid="`video-mapping-from-${index}`"
            :placeholder="t('admin.accounts.requestModel')"
            @input="emitExtra"
          />
          <Icon name="arrowRight" size="sm" class="shrink-0 text-gray-400" />
          <input
            v-model="mapping.to"
            type="text"
            class="input flex-1"
            :data-testid="`video-mapping-to-${index}`"
            :placeholder="t('admin.accounts.actualModel')"
            @input="emitExtra"
          />
          <button
            type="button"
            class="rounded-lg p-2 text-red-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20"
            :aria-label="t('common.delete')"
            @click="removeMapping(index)"
          >
            <Icon name="trash" size="sm" />
          </button>
        </div>
      </div>
      <button
        type="button"
        data-testid="video-add-mapping"
        class="mt-3 w-full rounded-lg border-2 border-dashed border-gray-300 px-4 py-2 text-sm text-gray-600 transition-colors hover:border-gray-400 hover:text-gray-700 dark:border-dark-500 dark:text-gray-400 dark:hover:border-dark-400 dark:hover:text-gray-300"
        @click="addMapping"
      >
        <Icon name="plus" size="sm" class="mr-1 inline" />
        {{ t('admin.accounts.addMapping') }}
      </button>
    </div>

    <details class="rounded-lg border border-gray-200 dark:border-dark-600" open>
      <summary class="cursor-pointer px-4 py-3 text-sm font-medium text-gray-700 dark:text-gray-300">
        {{ t('admin.accounts.video.capabilities') }}
      </summary>
      <div class="border-t border-gray-200 p-4 dark:border-dark-600">
        <template v-if="safeCapabilityTags.length > 0">
          <div class="mb-3 flex flex-wrap gap-2">
            <span
              v-for="capability in safeCapabilityTags"
              :key="capability"
              :data-testid="`video-capability-${capability}`"
              class="inline-flex rounded-full bg-primary-50 px-2.5 py-1 text-xs font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300"
            >
              {{ t(`admin.accounts.video.capability.${capability}`) }}
            </span>
          </div>
          <div class="space-y-2">
            <label v-for="capability in safeCapabilityTags" :key="`disable-${capability}`" class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
              <input
                type="checkbox"
                :checked="disabledCapabilities.includes(capability)"
                :data-testid="`video-disable-${capability}`"
                class="rounded border-gray-300 text-primary-600 focus:ring-primary-500 dark:border-dark-500"
                @change="toggleCapability(capability, $event)"
              />
              {{ t('admin.accounts.video.disableCapability', { capability: t(`admin.accounts.video.capability.${capability}`) }) }}
            </label>
          </div>
        </template>
        <p v-else class="text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.video.capabilitiesUnavailable') }}
        </p>
      </div>
    </details>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import { buildVideoCredentials, type VideoProvider } from './credentialsBuilder'

const props = withDefaults(defineProps<{
  mode?: 'create' | 'edit'
  provider: VideoProvider
  credentials: Record<string, unknown>
  extra: Record<string, unknown>
  credentialStatus?: Record<string, boolean>
  capabilityTags?: string[]
}>(), {
  mode: 'create',
  credentialStatus: () => ({}),
  capabilityTags: () => []
})

const emit = defineEmits<{
  'update:provider': [provider: VideoProvider]
  'update:credentials': [credentials: Record<string, string>]
  'update:extra': [extra: Record<string, unknown>]
}>()

const { t } = useI18n()
const providerOptions = computed(() => [
  { value: 'seedance', label: t('admin.accounts.video.providers.seedance') },
  { value: 'kling', label: t('admin.accounts.video.providers.kling') }
])

const apiKey = ref('')
const accessKey = ref('')
const secretKey = ref('')
const baseUrl = ref('')
let nextMappingID = 1
const mappings = ref<Array<{ id: number; from: string; to: string }>>([])

const readString = (value: unknown) => typeof value === 'string' ? value : ''

const syncFromProps = () => {
  apiKey.value = props.mode === 'create' ? readString(props.credentials.api_key) : ''
  accessKey.value = props.mode === 'create' ? readString(props.credentials.access_key) : ''
  secretKey.value = props.mode === 'create' ? readString(props.credentials.secret_key) : ''
  baseUrl.value = readString(props.credentials.base_url)
  const rawMapping = props.extra.model_mapping
  mappings.value = rawMapping && typeof rawMapping === 'object' && !Array.isArray(rawMapping)
    ? Object.entries(rawMapping as Record<string, unknown>).map(([from, to]) => ({
      id: nextMappingID++,
      from,
      to: readString(to)
    }))
    : []
}

// The component owns the draft while mounted. Re-syncing after each v-model emit
// would erase a newly-added blank mapping row before the user can fill it.
syncFromProps()

const disabledCapabilities = computed(() => Array.isArray(props.extra.video_disabled_capabilities)
  ? props.extra.video_disabled_capabilities.filter((value): value is string => typeof value === 'string')
  : [])
const knownCapabilityTags = new Set([
  'audio', 'edit', 'extension', 'first_and_last_frame', 'first_frame',
  'generation', 'last_frame', 'reference_images', 'reference_videos', 'text'
])
const safeCapabilityTags = computed(() => Array.from(new Set(
  props.capabilityTags.filter(tag => typeof tag === 'string' && knownCapabilityTags.has(tag))
)))

const currentCredentials = () => buildVideoCredentials({
  platform: 'video',
  provider: props.provider,
  apiKey: apiKey.value,
  accessKey: accessKey.value,
  secretKey: secretKey.value,
  baseUrl: baseUrl.value
})

const mappingObject = () => Object.fromEntries(
  mappings.value
    .map(mapping => [mapping.from.trim(), mapping.to.trim()] as const)
    .filter(([from, to]) => from && to)
)

const emitCredentials = () => emit('update:credentials', currentCredentials())
const handleCredentialInput = (
  field: 'api_key' | 'access_key' | 'secret_key' | 'base_url',
  event: Event
) => {
  const value = (event.target as HTMLInputElement).value
  if (field === 'api_key') apiKey.value = value
  if (field === 'access_key') accessKey.value = value
  if (field === 'secret_key') secretKey.value = value
  if (field === 'base_url') baseUrl.value = value
  emitCredentials()
}
const emitExtra = () => emit('update:extra', {
  model_mapping: mappingObject(),
  ...(disabledCapabilities.value.length > 0
    ? { video_disabled_capabilities: [...disabledCapabilities.value] }
    : {})
})

const handleProviderChange = (value: string | number | boolean | null) => {
  if (value !== 'seedance' && value !== 'kling') return
  apiKey.value = ''
  accessKey.value = ''
  secretKey.value = ''
  baseUrl.value = ''
  mappings.value = []
  emit('update:provider', value)
  emit('update:credentials', {})
  emit('update:extra', { model_mapping: {} })
}

const addMapping = () => mappings.value.push({ id: nextMappingID++, from: '', to: '' })
const removeMapping = (index: number) => {
  mappings.value.splice(index, 1)
  emitExtra()
}
const toggleCapability = (capability: string, event: Event) => {
  const checked = (event.target as HTMLInputElement).checked
  const next = checked
    ? Array.from(new Set([...disabledCapabilities.value, capability]))
    : disabledCapabilities.value.filter(value => value !== capability)
  emit('update:extra', {
    model_mapping: mappingObject(),
    ...(next.length > 0 ? { video_disabled_capabilities: next } : {})
  })
}
</script>
