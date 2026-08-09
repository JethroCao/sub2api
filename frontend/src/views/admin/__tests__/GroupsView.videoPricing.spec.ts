import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { AdminGroup } from '@/types'
import GroupsView from '@/views/admin/GroupsView.vue'

const {
  listGroups,
  createGroup,
  updateGroup,
  listVideoPricingRules,
  replaceVideoPricingRules,
  listAccounts,
  getModelsListCandidates,
  getLiveCapability,
  showSuccess,
  showError
} = vi.hoisted(() => ({
  listGroups: vi.fn(),
  createGroup: vi.fn(),
  updateGroup: vi.fn(),
  listVideoPricingRules: vi.fn(),
  replaceVideoPricingRules: vi.fn(),
  listAccounts: vi.fn(),
  getModelsListCandidates: vi.fn(),
  getLiveCapability: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    groups: {
      list: listGroups,
      create: createGroup,
      update: updateGroup,
      listVideoPricingRules,
      replaceVideoPricingRules,
      getModelsListCandidates,
      getLiveCapability,
      getUsageSummary: vi.fn().mockResolvedValue([]),
      getCapacitySummary: vi.fn().mockResolvedValue([]),
      getAll: vi.fn().mockResolvedValue([]),
      duplicate: vi.fn(),
      delete: vi.fn(),
      updateSortOrder: vi.fn()
    },
    accounts: { list: listAccounts, getById: vi.fn() }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess, showError })
}))

vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: vi.fn(() => false), nextStep: vi.fn() })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${Object.values(params).join(':')}` : key
    })
  }
})

const videoGroup: AdminGroup = {
  id: 42,
  name: 'Video group',
  description: null,
  platform: 'video',
  rate_multiplier: 1,
  rpm_limit: 0,
  is_exclusive: false,
  status: 'active',
  subscription_type: 'standard',
  daily_limit_usd: null,
  weekly_limit_usd: null,
  monthly_limit_usd: null,
  allow_image_generation: true,
  allow_video_generation: false,
  allow_batch_image_generation: false,
  image_rate_independent: false,
  image_rate_multiplier: 1,
  batch_image_discount_multiplier: 0.5,
  batch_image_hold_multiplier: 0.6,
  image_price_1k: null,
  image_price_2k: null,
  image_price_4k: null,
  video_rate_independent: false,
  video_rate_multiplier: 1,
  video_price_480p: null,
  video_price_720p: null,
  video_price_1080p: null,
  web_search_price_per_call: null,
  peak_rate_enabled: false,
  peak_start: '',
  peak_end: '',
  peak_rate_multiplier: 1,
  claude_code_only: false,
  fallback_group_id: null,
  fallback_group_id_on_invalid_request: null,
  allow_messages_dispatch: false,
  default_mapped_model: '',
  messages_dispatch_model_config: undefined,
  require_oauth_only: false,
  require_privacy_set: false,
  created_at: '2026-08-09T00:00:00Z',
  updated_at: '2026-08-09T00:00:00Z',
  model_routing: null,
  model_routing_enabled: false,
  mcp_xml_inject: true,
  supported_model_scopes: [],
  account_count: 1,
  active_account_count: 1,
  rate_limited_account_count: 0,
  models_list_config: undefined,
  sort_order: 10
}

const pricingRule = {
  id: 9,
  group_id: 42,
  external_model: 'seedance-2.0',
  operation: 'generation' as const,
  resolution: '*' as const,
  audio_mode: 'any' as const,
  unit: 'per_output_second' as const,
  unit_price: 0.1,
  upstream_unit_cost: null,
  enabled: true
}

const AppLayoutStub = defineComponent({ template: '<main><slot /></main>' })
const TablePageLayoutStub = defineComponent({
  template: '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>'
})
const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: '<div><div v-for="row in data" :key="row.id" :data-testid="`group-row-${row.id}`"><slot name="cell-actions" :row="row" /></div></div>'
})
const SelectStub = defineComponent({
  inheritAttrs: false,
  props: ['modelValue', 'options', 'disabled'],
  emits: ['update:modelValue', 'change'],
  template: `<select v-bind="$attrs" :value="modelValue" :disabled="disabled"
    @change="$emit('update:modelValue', $event.target.value); $emit('change')">
    <option v-for="option in options" :key="String(option.value)" :value="option.value">{{ option.label }}</option>
  </select>`
})
const BaseDialogStub = defineComponent({
  props: ['show'],
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})
const VideoPricingRulesEditorStub = defineComponent({
  props: ['modelValue', 'capabilities'],
  emits: ['update:modelValue'],
  setup(_props, { expose }) {
    expose({ validate: () => true })
  },
  template: '<div data-testid="video-editor">{{ modelValue.map(rule => rule.external_model).join(",") }}</div>'
})

function mountView(options: { realVideoPricingEditor?: boolean } = {}) {
  return mount(GroupsView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: DataTableStub,
        Pagination: true,
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        EmptyState: true,
        Select: SelectStub,
        PlatformIcon: true,
        Icon: true,
        GroupCapacityBadge: true,
        GroupRateMultipliersModal: true,
        GroupRPMOverridesModal: true,
        ...(options.realVideoPricingEditor
          ? {}
          : { VideoPricingRulesEditor: VideoPricingRulesEditorStub }),
        TotpStepUpDialog: true,
        VueDraggable: { template: '<div><slot /></div>' }
      }
    }
  })
}

function findCopySourceSelect(
  wrapper: ReturnType<typeof mount>,
  formSelector: '#create-group-form' | '#edit-group-form',
  groupID: number
) {
  const select = wrapper.findAll(`${formSelector} select`).find(candidate =>
    candidate.find(`option[value="${groupID}"]`).exists()
  )
  if (!select) throw new Error(`copy-account source ${groupID} not found in ${formSelector}`)
  return select
}

async function openEdit(wrapper: ReturnType<typeof mount>) {
  const edit = wrapper.findAll('button').find(button => button.text() === 'common.edit')
  if (!edit) throw new Error('edit action not found')
  await edit.trigger('click')
  await flushPromises()
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise
    reject = rejectPromise
  })
  return { promise, resolve, reject }
}

describe('GroupsView video pricing safety and permissions', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    for (const fn of [
      listGroups, createGroup, updateGroup, listVideoPricingRules,
      replaceVideoPricingRules, listAccounts, getModelsListCandidates,
      getLiveCapability, showSuccess, showError
    ]) fn.mockReset()

    listGroups.mockResolvedValue({ items: [videoGroup], total: 1, page: 1, page_size: 20, pages: 1 })
    createGroup.mockResolvedValue({ ...videoGroup, id: 43, allow_image_generation: false, allow_video_generation: true })
    updateGroup.mockResolvedValue(videoGroup)
    listVideoPricingRules.mockResolvedValue([pricingRule])
    replaceVideoPricingRules.mockResolvedValue([])
    listAccounts.mockResolvedValue({
      items: [{
        id: 5,
        name: 'Seedance',
        platform: 'video',
        type: 'apikey',
        status: 'active',
        video_provider: 'seedance',
        video_capabilities: ['generation'],
        extra: { model_mapping: { 'seedance-2.0': 'endpoint-id' } }
      }],
      total: 1,
      page: 1,
      page_size: 100,
      pages: 1
    })
    getModelsListCandidates.mockResolvedValue([])
    getLiveCapability.mockResolvedValue({ supported: false })
  })

  afterEach(() => vi.restoreAllMocks())

  it('retains loaded pricing and blocks group save until authoritative data reloads successfully', async () => {
    listAccounts.mockRejectedValueOnce(new Error('secret account response'))
    const wrapper = mountView()
    await flushPromises()

    await openEdit(wrapper)

    expect(wrapper.get('[data-testid="video-editor"]').text()).toBe('seedance-2.0')
    const submit = wrapper.get('button[form="edit-group-form"]')
    expect(submit.attributes('disabled')).toBeDefined()
    await wrapper.get('#edit-group-form').trigger('submit')
    expect(updateGroup).not.toHaveBeenCalled()
    expect(replaceVideoPricingRules).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.groups.videoPricing.errors.loadFailed')
    expect(showError).not.toHaveBeenCalledWith(expect.stringContaining('secret'))

    await wrapper.get('[data-testid="retry-edit-video-pricing"]').trigger('click')
    await flushPromises()
    expect(listVideoPricingRules).toHaveBeenCalledTimes(2)
    expect(listAccounts).toHaveBeenCalledTimes(2)
    expect(wrapper.get('button[form="edit-group-form"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('initializes image and video permissions independently and preserves both in edit payloads', async () => {
    const wrapper = mountView()
    await flushPromises()

    await openEdit(wrapper)
    expect((wrapper.get('[data-testid="edit-allow-video-generation"]').element as HTMLInputElement).checked).toBe(false)
    await wrapper.get('#edit-group-form').trigger('submit')
    await flushPromises()

    expect(updateGroup.mock.calls[0][1]).toMatchObject({
      allow_image_generation: true,
      allow_video_generation: false
    })
    wrapper.unmount()
  })

  it('submits a video permission that differs from image permission when creating a group', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
    await wrapper.get('#create-group-form input[required]').setValue('Independent video')
    await wrapper.get('#create-group-form [data-tour="group-form-platform"]').setValue('video')
    await wrapper.get('[data-testid="create-allow-video-generation"]').setValue(true)
    await wrapper.get('#create-group-form').trigger('submit')
    await flushPromises()

    expect(createGroup.mock.calls[0][0]).toMatchObject({
      allow_image_generation: false,
      allow_video_generation: true
    })
    wrapper.unmount()
  })

  it('blocks an immediate create submit while copied-source capabilities are pending', async () => {
    const pendingAccounts = deferred<Awaited<ReturnType<typeof listAccounts>>>()
    listAccounts.mockReturnValueOnce(pendingAccounts.promise)
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
    await wrapper.get('#create-group-form input[required]').setValue('Pending capabilities')
    await wrapper.get('#create-group-form [data-tour="group-form-platform"]').setValue('video')
    await findCopySourceSelect(wrapper, '#create-group-form', 42).setValue('42')
    await flushPromises()

    expect(wrapper.get('button[form="create-group-form"]').attributes('disabled')).toBeDefined()
    await wrapper.get('#create-group-form').trigger('submit')
    expect(createGroup).not.toHaveBeenCalled()
    expect(replaceVideoPricingRules).not.toHaveBeenCalled()

    pendingAccounts.resolve({
      items: [{
        id: 5,
        name: 'Seedance',
        platform: 'video',
        type: 'apikey',
        status: 'active',
        video_provider: 'seedance',
        video_capabilities: ['generation'],
        extra: { model_mapping: { 'seedance-2.0': 'endpoint-id' } }
      }],
      total: 1,
      page: 1,
      page_size: 100,
      pages: 1
    })
    await flushPromises()
    expect(wrapper.get('button[form="create-group-form"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('keeps create blocked after a source failure and safely retries the latest selection', async () => {
    listAccounts
      .mockRejectedValueOnce(new Error('credential=do-not-render'))
      .mockResolvedValueOnce({
        items: [{
          id: 6,
          name: 'Safe source',
          platform: 'video',
          type: 'apikey',
          status: 'active',
          video_provider: 'seedance',
          video_capabilities: ['generation'],
          extra: { model_mapping: { 'seedance-safe': 'endpoint-safe' } }
        }],
        total: 1,
        page: 1,
        page_size: 100,
        pages: 1
      })
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
    await wrapper.get('#create-group-form input[required]').setValue('Retry safely')
    await wrapper.get('#create-group-form [data-tour="group-form-platform"]').setValue('video')
    await findCopySourceSelect(wrapper, '#create-group-form', 42).setValue('42')
    await flushPromises()

    expect(wrapper.get('button[form="create-group-form"]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="retry-create-video-pricing"]').exists()).toBe(true)
    expect(showError).toHaveBeenCalledWith('admin.groups.videoPricing.errors.loadFailed')
    expect(showError).not.toHaveBeenCalledWith(expect.stringContaining('credential'))
    await wrapper.get('#create-group-form').trigger('submit')
    expect(createGroup).not.toHaveBeenCalled()
    expect(replaceVideoPricingRules).not.toHaveBeenCalled()

    await wrapper.get('[data-testid="retry-create-video-pricing"]').trigger('click')
    await flushPromises()
    expect(listAccounts).toHaveBeenCalledTimes(2)
    expect(wrapper.get('button[form="create-group-form"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('uses replacement-source capabilities for edit validation and replacement', async () => {
    const sourceGroup = { ...videoGroup, id: 84, name: 'Video source', account_count: 1 }
    listGroups.mockResolvedValueOnce({
      items: [videoGroup, sourceGroup], total: 2, page: 1, page_size: 20, pages: 1
    })
    listAccounts.mockImplementation((_page, _pageSize, filters) => Promise.resolve({
      items: filters.group === '84'
        ? [{
            id: 84,
            name: 'Replacement source',
            platform: 'video',
            type: 'apikey',
            status: 'active',
            video_provider: 'seedance',
            video_capabilities: ['generation'],
            extra: { model_mapping: { 'seedance-source': 'endpoint-source' } }
          }]
        : [{
            id: 42,
            name: 'Current target',
            platform: 'video',
            type: 'apikey',
            status: 'active',
            video_provider: 'seedance',
            video_capabilities: ['generation'],
            extra: { model_mapping: { 'seedance-2.0': 'endpoint-current' } }
          }],
      total: 1,
      page: 1,
      page_size: 100,
      pages: 1
    }))
    const wrapper = mountView({ realVideoPricingEditor: true })
    await flushPromises()
    await openEdit(wrapper)

    await findCopySourceSelect(wrapper, '#edit-group-form', 84).setValue('84')
    await flushPromises()
    await wrapper.get('#edit-group-form').trigger('submit')
    expect(updateGroup).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.groups.videoPricing.errors.validation')

    await wrapper.get('[data-testid="video-pricing-model-0"]').setValue('seedance-source')
    await wrapper.get('#edit-group-form').trigger('submit')
    await flushPromises()
    expect(updateGroup.mock.calls[0][1]).toMatchObject({
      copy_accounts_from_group_ids: [84]
    })
    expect(replaceVideoPricingRules.mock.calls[0][1][0]).toMatchObject({
      external_model: 'seedance-source',
      operation: 'generation'
    })
    wrapper.unmount()
  })

  it('ignores stale source capabilities after a newer edit source selection succeeds', async () => {
    const sourceB = { ...videoGroup, id: 84, name: 'Source B', account_count: 1 }
    const sourceC = { ...videoGroup, id: 85, name: 'Source C', account_count: 1 }
    listGroups.mockResolvedValueOnce({
      items: [videoGroup, sourceB, sourceC], total: 3, page: 1, page_size: 20, pages: 1
    })
    const staleB = deferred<Awaited<ReturnType<typeof listAccounts>>>()
    let sourceBCalls = 0
    listAccounts.mockImplementation((_page, _pageSize, filters) => {
      if (filters.group === '42') return Promise.resolve({
        items: [{
          id: 42, name: 'Target', platform: 'video', type: 'apikey', status: 'active',
          video_provider: 'seedance', video_capabilities: ['generation'],
          extra: { model_mapping: { 'seedance-2.0': 'target' } }
        }], total: 1, page: 1, page_size: 100, pages: 1
      })
      if (filters.group === '84') {
        sourceBCalls += 1
        if (sourceBCalls === 1) return staleB.promise
        return Promise.resolve({
          items: [{
            id: 84, name: 'B', platform: 'video', type: 'apikey', status: 'active',
            video_provider: 'seedance', video_capabilities: ['generation'],
            extra: { model_mapping: { 'seedance-b': 'b' } }
          }], total: 1, page: 1, page_size: 100, pages: 1
        })
      }
      return Promise.resolve({
        items: [{
          id: 85, name: 'C', platform: 'video', type: 'apikey', status: 'active',
          video_provider: 'seedance', video_capabilities: ['generation'],
          extra: { model_mapping: { 'seedance-c': 'c' } }
        }], total: 1, page: 1, page_size: 100, pages: 1
      })
    })
    const wrapper = mountView({ realVideoPricingEditor: true })
    await flushPromises()
    await openEdit(wrapper)

    const sourceSelect = findCopySourceSelect(wrapper, '#edit-group-form', 84)
    await sourceSelect.setValue('84')
    await sourceSelect.setValue('85')
    await flushPromises()
    expect(wrapper.get('[data-testid="video-pricing-model-0"] option[value="seedance-c"]').exists()).toBe(true)

    staleB.resolve({
      items: [{
        id: 84, name: 'Late B', platform: 'video', type: 'apikey', status: 'active',
        video_provider: 'seedance', video_capabilities: ['generation'],
        extra: { model_mapping: { 'seedance-late': 'late' } }
      }], total: 1, page: 1, page_size: 100, pages: 1
    })
    await flushPromises()
    expect(wrapper.get('[data-testid="video-pricing-model-0"] option[value="seedance-c"]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('blocks edit when the pricing rule list cannot be read authoritatively', async () => {
    listVideoPricingRules.mockRejectedValueOnce({
      code: 'VIDEO_PRICING_RULES_RESPONSE_INVALID'
    })
    const wrapper = mountView()
    await flushPromises()
    await openEdit(wrapper)

    expect(wrapper.get('button[form="edit-group-form"]').attributes('disabled')).toBeDefined()
    await wrapper.get('#edit-group-form').trigger('submit')
    expect(updateGroup).not.toHaveBeenCalled()
    expect(replaceVideoPricingRules).not.toHaveBeenCalled()
    expect(showError).toHaveBeenCalledWith('admin.groups.videoPricing.errors.loadFailed')
    wrapper.unmount()
  })

  it('keeps a created group and reports safe partial success when pricing save fails', async () => {
    replaceVideoPricingRules.mockRejectedValueOnce(new Error('signed_response=secret'))
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-tour="groups-create-btn"]').trigger('click')
    await wrapper.get('#create-group-form input[required]').setValue('Partial create')
    await wrapper.get('#create-group-form [data-tour="group-form-platform"]').setValue('video')
    await wrapper.get('#create-group-form').trigger('submit')
    await flushPromises()

    expect(createGroup).toHaveBeenCalledTimes(1)
    expect(replaceVideoPricingRules).toHaveBeenCalledWith(43, [])
    expect(listGroups.mock.calls.length).toBeGreaterThanOrEqual(2)
    expect(showError).toHaveBeenCalledWith(
      'admin.groups.videoPricing.createPartialSuccess:admin.groups.videoPricing.errors.generic'
    )
    expect(showError).not.toHaveBeenCalledWith(expect.stringContaining('signed_response'))
    wrapper.unmount()
  })

  it('ignores a late group-A retry after group B becomes the active edit', async () => {
    const groupB = { ...videoGroup, id: 84, name: 'Video group B' }
    const groupBRule = {
      ...pricingRule,
      id: 19,
      group_id: 84,
      external_model: 'seedance-b'
    }
    listGroups.mockResolvedValueOnce({
      items: [videoGroup, groupB], total: 2, page: 1, page_size: 20, pages: 1
    })

    const lateARules = deferred<typeof pricingRule[]>()
    const lateAAccounts = deferred<Awaited<ReturnType<typeof listAccounts>>>()
    const groupBRules = deferred<typeof pricingRule[]>()
    const groupBAccounts = deferred<Awaited<ReturnType<typeof listAccounts>>>()
    let groupARuleCalls = 0
    let groupAAccountCalls = 0
    listVideoPricingRules.mockImplementation((groupID: number) => {
      if (groupID === 42) {
        groupARuleCalls += 1
        return groupARuleCalls === 1 ? Promise.resolve([pricingRule]) : lateARules.promise
      }
      return groupBRules.promise
    })
    listAccounts.mockImplementation((_page, _pageSize, filters) => {
      if (filters.group === '42') {
        groupAAccountCalls += 1
        if (groupAAccountCalls === 1) return Promise.reject(new Error('A initial load failed'))
        return lateAAccounts.promise
      }
      return groupBAccounts.promise
    })

    const wrapper = mountView()
    await flushPromises()
    await openEdit(wrapper)
    void wrapper.get('[data-testid="retry-edit-video-pricing"]').trigger('click')
    await flushPromises()

    const cancel = wrapper.findAll('button').find(button => button.text() === 'common.cancel')
    if (!cancel) throw new Error('edit cancel action not found')
    await cancel.trigger('click')
    const groupBEdit = wrapper.get('[data-testid="group-row-84"]').findAll('button')
      .find(button => button.text() === 'common.edit')
    if (!groupBEdit) throw new Error('group B edit action not found')
    void groupBEdit.trigger('click')
    groupBRules.resolve([groupBRule])
    groupBAccounts.reject(new Error('B authoritative accounts unavailable'))
    await flushPromises()

    expect(wrapper.get('[data-testid="video-editor"]').text()).toBe('seedance-b')
    expect(wrapper.get('button[form="edit-group-form"]').attributes('disabled')).toBeDefined()

    lateARules.resolve([{ ...pricingRule, external_model: 'seedance-a' }])
    lateAAccounts.resolve({
      items: [{
        id: 15,
        name: 'Late Seedance A',
        platform: 'video',
        type: 'apikey',
        status: 'active',
        video_provider: 'seedance',
        video_capabilities: ['generation'],
        extra: { model_mapping: { 'seedance-a': 'endpoint-a' } }
      }],
      total: 1,
      page: 1,
      page_size: 100,
      pages: 1
    })
    await flushPromises()

    expect(wrapper.get('[data-testid="video-editor"]').text()).toBe('seedance-b')
    expect(wrapper.get('button[form="edit-group-form"]').attributes('disabled')).toBeDefined()
    await wrapper.get('#edit-group-form').trigger('submit')
    expect(updateGroup).not.toHaveBeenCalled()
    expect(replaceVideoPricingRules).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
