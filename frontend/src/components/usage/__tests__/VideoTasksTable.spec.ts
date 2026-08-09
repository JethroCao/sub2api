import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

import VideoTasksTable from '../VideoTasksTable.vue'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

const task = {
  request_id: 'vid_00000000000000000000000000000001',
  user_id: 7,
  api_key_id: 8,
  group_id: 9,
  account_id: 10,
  provider: 'seedance',
  operation: 'generation',
  external_model: 'seedance-2.0',
  status: 'running',
  upstream_status: 'processing',
  result_url_summary: '',
  result_duration_seconds: null,
  result_width: null,
  result_height: null,
  pricing_unit: 'per_output_second',
  unit_price: 0.2,
  estimated_units: 5,
  upstream_unit_cost: 0.1,
  actual_upstream_cost: null,
  estimated_amount: 1,
  frozen_amount: 1,
  settled_amount: 0,
  billing_status: 'held',
  last_error_code: '',
  next_poll_at: null,
  lease_expires_at: null,
  created_at: '2026-08-09T01:00:00Z',
  updated_at: '2026-08-09T01:00:05Z',
  submitted_at: '2026-08-09T01:00:01Z',
  started_at: '2026-08-09T01:00:02Z',
  finished_at: null,
  settled_at: null,
}

const mountTable = () => mount(VideoTasksTable, {
  props: { tasks: [task], total: 1, loading: false, page: 1, pageSize: 20 },
  global: {
    stubs: {
      Icon: true,
      DataTable: {
        props: ['data'],
        emits: ['rowClick'],
        template: '<button data-testid="video-task-row" @click="$emit(\'rowClick\', data[0])">open</button>',
      },
      Pagination: true,
      Select: {
        name: 'Select',
        props: ['modelValue', 'options'],
        emits: ['update:modelValue'],
        template: '<button data-testid="select-filter" @click="$emit(\'update:modelValue\', \'seedance\')" />',
      },
      Input: {
        props: ['modelValue'],
        emits: ['update:modelValue'],
        template: '<input />',
      },
      DateRangePicker: true,
    },
  },
})

describe('VideoTasksTable', () => {
  it('opens the selected video task from the shared table pattern', async () => {
    const wrapper = mountTable()
    await wrapper.get('[data-testid="video-task-row"]').trigger('click')
    expect(wrapper.emitted('open')?.[0]).toEqual([task])
  })

  it('emits provider, model, operation, status, group, user, and date filters', async () => {
    const wrapper = mountTable()
    const vm = wrapper.vm as unknown as { draftFilters: Record<string, unknown>; applyFilters: () => void }
    Object.assign(vm.draftFilters, {
      provider: 'seedance',
      model: 'seedance-2.0',
      operation: 'generation',
      status: 'running',
      group_id: '9',
      user_id: '7',
      start_date: '2026-08-01',
      end_date: '2026-08-09',
    })
    vm.applyFilters()
    expect(wrapper.emitted('filter-change')?.[0]?.[0]).toEqual({
      provider: 'seedance',
      model: 'seedance-2.0',
      operation: 'generation',
      status: 'running',
      group_id: 9,
      user_id: 7,
      start_date: '2026-08-01',
      end_date: '2026-08-09',
    })
  })

  it('offers only status values accepted by the admin video API', () => {
    const wrapper = mountTable()
    const statusOptions = wrapper.findAllComponents({ name: 'Select' })[2]?.props('options') as Array<{ value: string }>
    expect(statusOptions.map((option) => option.value)).not.toContain('manual_review')
  })
})
