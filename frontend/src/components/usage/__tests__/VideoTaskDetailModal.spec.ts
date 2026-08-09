import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import VideoTaskDetailModal from '../VideoTaskDetailModal.vue'

const { refundVideoTask, reconcileVideoTask, completeVideoTask } = vi.hoisted(() => ({
  refundVideoTask: vi.fn(),
  reconcileVideoTask: vi.fn(),
  completeVideoTask: vi.fn(),
}))

vi.mock('@/api/admin/usage', () => ({
  adminUsageAPI: { refundVideoTask, reconcileVideoTask, completeVideoTask },
  default: { refundVideoTask, reconcileVideoTask, completeVideoTask },
}))

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('@/components/auth/TotpStepUpDialog.vue', () => ({ default: { template: '<div />' } }))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

const task = (overrides: Record<string, unknown> = {}) => ({
  request_id: 'vid_00000000000000000000000000000001',
  user_id: 7,
  api_key_id: 8,
  group_id: 9,
  account_id: 10,
  provider: 'seedance',
  operation: 'generation',
  external_model: 'seedance-2.0',
  status: 'unknown',
  upstream_status: 'unknown',
  result_url_summary: 'https://media.example.com/video-result?secret=never-render',
  result_url_expires_at: '2026-08-09T02:00:00Z',
  result_duration_seconds: null,
  result_width: null,
  result_height: null,
  pricing_unit: 'per_request',
  unit_price: 1,
  estimated_units: 1,
  upstream_unit_cost: null,
  actual_upstream_cost: null,
  estimated_amount: 1,
  frozen_amount: 1,
  settled_amount: 0,
  billing_status: 'held',
  last_error_code: 'UPSTREAM_FAILED',
  next_poll_at: null,
  lease_expires_at: null,
  created_at: '2026-08-09T01:00:00Z',
  updated_at: '2026-08-09T01:00:05Z',
  submitted_at: null,
  started_at: null,
  finished_at: null,
  settled_at: null,
  ...overrides,
})

const mountModal = (overrides: Record<string, unknown> = {}) => mount(VideoTaskDetailModal, {
  props: {
    show: true,
    detail: {
      task: task(overrides),
      result_url_summary: 'https://media.example.com/video-result?secret=never-render',
      events: [{
        id: 1,
        event_type: 'provider_error',
        payload: {
          status: 'failed',
          error_code: 'RATE_LIMIT',
          message: 'safe summary https://provider.example/video?signature=query-secret Bearer header-secret',
          prompt: 'never render prompt',
          authorization: 'Bearer secret',
          raw_payload: { api_key: 'secret' },
        },
        created_at: '2026-08-09T01:00:05Z',
      }],
    },
    loading: false,
  },
  global: {
    stubs: {
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot/><slot name="footer"/></div>' },
      ConfirmDialog: false,
      TotpStepUpDialog: true,
      Icon: true,
      Input: { props: ['modelValue'], emits: ['update:modelValue'], template: '<input />' },
      Select: true,
    },
  },
})

describe('VideoTaskDetailModal', () => {
  beforeEach(() => {
    vi.spyOn(globalThis.crypto, 'randomUUID').mockReturnValue('11111111-1111-4111-8111-111111111111')
    refundVideoTask.mockReset()
    reconcileVideoTask.mockReset()
    completeVideoTask.mockReset()
  })

  it('disables refund for captured tasks', () => {
    const wrapper = mountModal({ billing_status: 'captured', status: 'succeeded' })
    expect(wrapper.get('[data-testid="video-task-refund"]').attributes('disabled')).toBeDefined()
  })

  it('renders only sanitized event fields and the result host without signed query data', () => {
    const wrapper = mountModal()
    expect(wrapper.text()).toContain('media.example.com')
    expect(wrapper.text()).toContain('safe summary')
    expect(wrapper.text()).not.toContain('never-render')
    expect(wrapper.text()).not.toContain('Bearer secret')
    expect(wrapper.text()).not.toContain('api_key')
    expect(wrapper.text()).not.toContain('query-secret')
    expect(wrapper.text()).not.toContain('header-secret')
  })

  it('shows task lifecycle timestamps and upstream cost in the audited detail', () => {
    const wrapper = mountModal({
      submitted_at: '2026-08-09T01:00:01Z',
      started_at: '2026-08-09T01:00:02Z',
      finished_at: '2026-08-09T01:00:10Z',
      actual_upstream_cost: 0.4,
    })
    expect(wrapper.get('[data-testid="video-task-timestamps"]').text()).toContain('videoTasks.detail.submittedAt')
    expect(wrapper.text()).toContain('$0.4000')
  })

  it('creates an idempotency key when confirmation opens and reuses it for a failed retry', async () => {
    refundVideoTask
      .mockRejectedValueOnce(new Error('temporary'))
      .mockResolvedValueOnce({ task: task({ billing_status: 'released' }), replayed: true })
    const wrapper = mountModal()
    await wrapper.get('[data-testid="video-task-refund"]').trigger('click')
    const vm = wrapper.vm as unknown as { actionReason: string }
    vm.actionReason = 'provider confirmed absent'
    await wrapper.vm.$nextTick()
    await wrapper.get('[data-testid="video-task-confirm"]').trigger('click')
    await vi.waitFor(() => {
      expect(refundVideoTask).toHaveBeenCalledTimes(1)
      expect(wrapper.get('[data-testid="video-task-confirm"]').attributes('disabled')).toBeUndefined()
    })
    await wrapper.get('[data-testid="video-task-confirm"]').trigger('click')
    await flushPromises()

    expect(refundVideoTask).toHaveBeenCalledTimes(2)
    expect(refundVideoTask.mock.calls[0]?.[2]).toBe('video-refund-vid_00000000000000000000000000000001-11111111-1111-4111-8111-111111111111')
    expect(refundVideoTask.mock.calls[1]?.[2]).toBe(refundVideoTask.mock.calls[0]?.[2])
  })

  it('invalidates an open action when the target task changes during the request', async () => {
    let resolveRefund!: (value: unknown) => void
    refundVideoTask.mockReturnValue(new Promise((resolve) => { resolveRefund = resolve }))
    const wrapper = mountModal()
    await wrapper.get('[data-testid="video-task-refund"]').trigger('click')
    ;(wrapper.vm as unknown as { actionReason: string }).actionReason = 'provider confirmed absent'
    await wrapper.vm.$nextTick()
    await wrapper.get('[data-testid="video-task-confirm"]').trigger('click')
    await vi.waitFor(() => expect(refundVideoTask).toHaveBeenCalledTimes(1))

    await wrapper.setProps({
      detail: {
        ...(wrapper.props('detail') as Record<string, unknown>),
        task: task({ request_id: 'vid_00000000000000000000000000000002' }),
      },
    })
    expect(wrapper.find('[data-testid="video-task-confirm"]').exists()).toBe(false)

    resolveRefund({ task: task({ billing_status: 'released' }), replayed: false })
    await flushPromises()
    expect(wrapper.emitted('refreshed')).toBeUndefined()
  })
})
