import { describe, expect, it } from 'vitest'

import {
  buildVideoPricingPreview,
  deriveAuthoritativeVideoCapabilities,
  resolveVideoPricingRule,
  validateVideoPricingRules,
  videoPricingRulesForReplacement
} from '../groupsVideoPricing'

const rule = (overrides: Record<string, unknown> = {}) => ({
  id: 1,
  group_id: 7,
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

describe('group video pricing helpers', () => {
  it('selects the most specific enabled video pricing rule', () => {
    const rules = [
      rule({ id: 1, resolution: '*', audio_mode: 'any', unit_price: 0.1 }),
      rule({ id: 2, resolution: '1080p', audio_mode: 'with_audio', unit_price: 0.25 })
    ]

    expect(resolveVideoPricingRule(rules, {
      model: 'seedance-2.0',
      operation: 'generation',
      resolution: '1080p',
      audio: true
    })?.id).toBe(2)
  })

  it('mirrors backend dimensions, numeric checks, duplicate checks, and wildcard coverage', () => {
    expect(validateVideoPricingRules([
      rule({ id: 1, resolution: '720p', audio_mode: 'any' }),
      rule({ id: 2, resolution: '720p', audio_mode: 'any' })
    ], [{ external_model: 'seedance-2.0', operation: 'generation' }])).toEqual([
      { code: 'overlap', row: 1 },
      { code: 'coverage', external_model: 'seedance-2.0', operation: 'generation' }
    ])

    expect(validateVideoPricingRules([
      rule({ unit_price: Number.POSITIVE_INFINITY })
    ], [{ external_model: 'seedance-2.0', operation: 'generation' }])).toEqual([
      { code: 'invalid', row: 0 }
    ])
  })

  it('previews final customer totals at 5s and 10s and keeps unknown margin nullable', () => {
    expect(buildVideoPricingPreview(rule({ unit_price: 0.125 }))).toEqual({
      five_second_price: 0.625,
      ten_second_price: 1.25,
      five_second_margin: null,
      ten_second_margin: null
    })

    expect(buildVideoPricingPreview(rule({ unit_price: 0.125, upstream_unit_cost: 0.075 }))).toEqual({
      five_second_price: 0.625,
      ten_second_price: 1.25,
      five_second_margin: 0.25,
      ten_second_margin: 0.5
    })
  })

  it('derives coverage only from active accounts with effective API capability metadata', () => {
    const capabilities = deriveAuthoritativeVideoCapabilities([
      {
        platform: 'video',
        status: 'active',
        video_provider: 'seedance',
        video_capabilities: ['generation', 'audio'],
        extra: { model_mapping: { 'seedance-2.0': 'ep-seedance' } }
      },
      {
        platform: 'video',
        status: 'inactive',
        video_provider: 'seedance',
        video_capabilities: ['generation'],
        extra: { model_mapping: { 'seedance-inactive': 'ep-disabled' } }
      },
      {
        platform: 'video',
        status: 'active',
        video_provider: 'other',
        video_capabilities: ['generation'],
        extra: { model_mapping: { 'other-model': 'upstream' } }
      }
    ])

    expect(capabilities).toEqual([
      { external_model: 'seedance-2.0', operation: 'generation' }
    ])
  })

  it('does not present dormant Kling as available or require false pricing coverage', () => {
    const capabilities = deriveAuthoritativeVideoCapabilities([{
      platform: 'video',
      status: 'active',
      video_provider: 'kling',
      video_capabilities: ['generation', 'extension'],
      extra: { model_mapping: { 'kling-3.0': 'kling-v3' } }
    }])

    expect(capabilities).toEqual([])
    expect(validateVideoPricingRules([], capabilities)).toEqual([])
  })

  it('removes dormant Kling rows from every replacement payload', () => {
    expect(videoPricingRulesForReplacement([
      rule({ id: 1, group_id: 7, external_model: ' kling-3.0 ', enabled: false }),
      rule({ id: 2, group_id: 7, external_model: 'seedance-2.0', unit_price: 0.2 })
    ])).toEqual([{
      external_model: 'seedance-2.0',
      operation: 'generation',
      resolution: '*',
      audio_mode: 'any',
      unit: 'per_output_second',
      unit_price: 0.2,
      upstream_unit_cost: null,
      enabled: true
    }])
  })

  it('fails closed on malformed mappings and unknown capability tags', () => {
    expect(deriveAuthoritativeVideoCapabilities([
      {
        platform: 'video',
        status: 'active',
        video_provider: 'seedance',
        video_capabilities: ['generation', 'unknown-operation'],
        extra: { model_mapping: { 'seedance-2.0': 'ep-seedance' } }
      },
      {
        platform: 'video',
        status: 'active',
        video_provider: 'seedance',
        video_capabilities: ['generation'],
        extra: { model_mapping: { 'seedance-2.0': 42 } }
      }
    ])).toEqual([])
  })
})
