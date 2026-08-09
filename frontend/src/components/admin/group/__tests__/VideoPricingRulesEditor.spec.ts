import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import VideoPricingRulesEditor from '../VideoPricingRulesEditor.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, params?: Record<string, unknown>) =>
      params ? `${key}:${Object.values(params).join(':')}` : key
  })
}))

const rule = (overrides: Record<string, unknown> = {}) => ({
  external_model: 'seedance-2.0',
  operation: 'generation' as const,
  resolution: '*' as const,
  audio_mode: 'any' as const,
  unit: 'per_output_second' as const,
  unit_price: 0.1,
  upstream_unit_cost: null,
  enabled: true,
  ...overrides
})

describe('VideoPricingRulesEditor', () => {
  it('emits an explicit warning for each authoritative model capability without coverage', () => {
    const wrapper = mount(VideoPricingRulesEditor, {
      props: {
        modelValue: [],
        capabilities: [{ external_model: 'seedance-2.0', operation: 'generation' }]
      }
    })

    expect(wrapper.get('[data-testid="video-pricing-missing"]').text()).toContain('seedance-2.0')
    expect(wrapper.get('[data-testid="video-pricing-missing"]').text()).toContain('generation')
  })

  it('edits every backend pricing dimension and enabled state', async () => {
    const wrapper = mount(VideoPricingRulesEditor, {
      props: {
        modelValue: [rule()],
        capabilities: [{ external_model: 'seedance-2.0', operation: 'generation' }]
      }
    })

    await wrapper.get('[data-testid="video-pricing-operation-0"]').setValue('edit')
    await wrapper.get('[data-testid="video-pricing-resolution-0"]').setValue('1080p')
    await wrapper.get('[data-testid="video-pricing-audio-0"]').setValue('with_audio')
    await wrapper.get('[data-testid="video-pricing-unit-0"]').setValue('per_request')
    await wrapper.get('[data-testid="video-pricing-unit-price-0"]').setValue('1.25')
    await wrapper.get('[data-testid="video-pricing-upstream-cost-0"]').setValue('0.75')
    await wrapper.get('[data-testid="video-pricing-enabled-0"]').setValue(false)

    expect(wrapper.emitted('update:modelValue')?.at(-1)?.[0]).toEqual([
      {
        external_model: 'seedance-2.0',
        operation: 'edit',
        resolution: '1080p',
        audio_mode: 'with_audio',
        unit: 'per_request',
        unit_price: 1.25,
        upstream_unit_cost: 0.75,
        enabled: false
      }
    ])
  })

  it('shows 5s and 10s customer previews and hides margin when upstream cost is unknown', () => {
    const wrapper = mount(VideoPricingRulesEditor, {
      props: {
        modelValue: [rule({ unit_price: 0.125 })],
        capabilities: [{ external_model: 'seedance-2.0', operation: 'generation' }]
      }
    })

    expect(wrapper.get('[data-testid="video-pricing-preview-0"]').text()).toContain('$0.625')
    expect(wrapper.get('[data-testid="video-pricing-preview-0"]').text()).toContain('$1.25')
    expect(wrapper.find('[data-testid="video-pricing-margin-0"]').exists()).toBe(false)
  })

  it('shows a margin preview only when upstream cost is present', () => {
    const wrapper = mount(VideoPricingRulesEditor, {
      props: {
        modelValue: [rule({ unit_price: 0.125, upstream_unit_cost: 0.075 })],
        capabilities: [{ external_model: 'seedance-2.0', operation: 'generation' }]
      }
    })

    expect(wrapper.get('[data-testid="video-pricing-margin-0"]').text()).toContain('$0.25')
    expect(wrapper.get('[data-testid="video-pricing-margin-0"]').text()).toContain('$0.5')
  })

  it('never renders a dormant legacy Kling rule or preserves it as an option', () => {
    const wrapper = mount(VideoPricingRulesEditor, {
      props: {
        modelValue: [rule({ external_model: 'kling-3.0', enabled: false })],
        capabilities: [{ external_model: 'seedance-2.0', operation: 'generation' }]
      }
    })

    expect(wrapper.find('[data-testid="video-pricing-model-0"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('kling-3.0')
  })
})
