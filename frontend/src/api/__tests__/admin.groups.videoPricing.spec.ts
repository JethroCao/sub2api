import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { get, put }
}))

import { listVideoPricingRules, replaceVideoPricingRules } from '@/api/admin/groups'

const rules = [{
  external_model: 'seedance-2.0',
  operation: 'generation' as const,
  resolution: '*' as const,
  audio_mode: 'any' as const,
  unit: 'per_output_second' as const,
  unit_price: 0.1,
  upstream_unit_cost: null,
  enabled: true
}]

describe('admin group video pricing API', () => {
  beforeEach(() => {
    get.mockReset()
    put.mockReset()
    vi.spyOn(globalThis.crypto, 'randomUUID').mockReturnValue('11111111-1111-4111-8111-111111111111')
  })

  it('normalizes the backend pricing response without exposing unrelated fields', async () => {
    get.mockResolvedValue({ data: [{
      ID: 9,
      GroupID: 7,
      ExternalModel: 'seedance-2.0',
      Operation: 'generation',
      Resolution: '*',
      AudioMode: 'any',
      Unit: 'per_output_second',
      UnitPrice: 0.1,
      UpstreamUnitCost: null,
      Enabled: true,
      Credential: 'must-not-survive'
    }] })

    await expect(listVideoPricingRules(7)).resolves.toEqual([{ id: 9, group_id: 7, ...rules[0] }])
  })

  it('reuses one idempotency key when the same logical replacement is retried after step-up', async () => {
    put.mockRejectedValueOnce({ status: 403, code: 'STEP_UP_REQUIRED' })
    put.mockResolvedValueOnce({ data: [] })

    await expect(replaceVideoPricingRules(7, rules)).rejects.toEqual({
      status: 403,
      code: 'STEP_UP_REQUIRED'
    })
    await replaceVideoPricingRules(7, rules)

    expect(put).toHaveBeenCalledTimes(2)
    expect(put.mock.calls[0][2].headers).toEqual(put.mock.calls[1][2].headers)
    expect(put.mock.calls[1][2].headers).toEqual({
      'Idempotency-Key': 'group-video-pricing-7-11111111-1111-4111-8111-111111111111'
    })
    expect(put.mock.calls[1][1]).toEqual({ rules })
  })

  it('uses a new idempotency key for a changed replacement payload', async () => {
    put.mockRejectedValueOnce({ status: 409, code: 'IDEMPOTENCY_KEY_CONFLICT' })
    await expect(replaceVideoPricingRules(7, rules)).rejects.toMatchObject({ status: 409 })

    vi.mocked(globalThis.crypto.randomUUID).mockReturnValueOnce('22222222-2222-4222-8222-222222222222')
    put.mockResolvedValueOnce({ data: [] })
    await replaceVideoPricingRules(7, [{ ...rules[0], unit_price: 0.2 }])

    expect(put.mock.calls[1][2].headers).toEqual({
      'Idempotency-Key': 'group-video-pricing-7-22222222-2222-4222-8222-222222222222'
    })
  })
})
