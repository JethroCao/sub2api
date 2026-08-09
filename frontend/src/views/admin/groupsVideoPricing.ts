import type {
  Account,
  VideoPricingCapability,
  VideoPricingOperation,
  VideoPricingRuleInput
} from '@/types'

export type VideoPricingValidationError =
  | { code: 'invalid' | 'overlap'; row: number }
  | { code: 'coverage'; external_model: string; operation: VideoPricingOperation }

const operations = new Set<VideoPricingOperation>(['generation', 'edit', 'extension'])
const resolutions = new Set(['*', '480p', '720p', '1080p'])
const audioModes = new Set(['any', 'with_audio', 'without_audio'])
const units = new Set(['per_request', 'per_output_second'])
const knownCapabilityTags = new Set([
  'audio', 'edit', 'extension', 'first_and_last_frame', 'first_frame',
  'generation', 'last_frame', 'reference_images', 'reference_videos', 'text'
])

const finiteNonNegative = (value: number) => Number.isFinite(value) && value >= 0

export function resolveVideoPricingRule<T extends VideoPricingRuleInput & { id?: number }>(
  rules: T[],
  query: { model: string; operation: VideoPricingOperation; resolution: string; audio: boolean }
): T | undefined {
  const audioMode = query.audio ? 'with_audio' : 'without_audio'
  let selected: T | undefined
  let selectedScore = -1
  for (const candidate of rules) {
    if (!candidate.enabled || candidate.external_model !== query.model || candidate.operation !== query.operation) continue
    if (candidate.resolution !== '*' && candidate.resolution !== query.resolution) continue
    if (candidate.audio_mode !== 'any' && candidate.audio_mode !== audioMode) continue
    const score = (candidate.resolution === query.resolution ? 2 : 0) +
      (candidate.audio_mode === audioMode ? 1 : 0)
    if (score > selectedScore) {
      selected = candidate
      selectedScore = score
    }
  }
  return selected
}

function validRule(rule: VideoPricingRuleInput): boolean {
  const model = rule.external_model.trim()
  return model.length > 0 && model.length <= 128 && !model.includes('\0') &&
    operations.has(rule.operation) && resolutions.has(rule.resolution) &&
    audioModes.has(rule.audio_mode) && units.has(rule.unit) &&
    finiteNonNegative(rule.unit_price) &&
    (rule.upstream_unit_cost === null || finiteNonNegative(rule.upstream_unit_cost))
}

export function validateVideoPricingRules(
  rules: VideoPricingRuleInput[],
  capabilities: VideoPricingCapability[]
): VideoPricingValidationError[] {
  if (rules.length > 1000) return [{ code: 'invalid', row: 1000 }]
  const errors: VideoPricingValidationError[] = []
  const seen = new Set<string>()
  rules.forEach((rule, row) => {
    if (!validRule(rule)) {
      errors.push({ code: 'invalid', row })
      return
    }
    const dimension = [rule.external_model.trim(), rule.operation, rule.resolution, rule.audio_mode].join('\0')
    if (seen.has(dimension)) errors.push({ code: 'overlap', row })
    else seen.add(dimension)
  })

  const authoritative = new Set(capabilities.map(({ external_model, operation }) => `${external_model}\0${operation}`))
  for (const rule of rules) {
    if (rule.enabled && !authoritative.has(`${rule.external_model.trim()}\0${rule.operation}`)) {
      const row = rules.indexOf(rule)
      if (!errors.some(error => 'row' in error && error.row === row)) errors.push({ code: 'invalid', row })
    }
  }
  for (const capability of capabilities) {
    const covered = rules.some(rule => rule.enabled &&
      rule.external_model.trim() === capability.external_model &&
      rule.operation === capability.operation &&
      rule.resolution === '*' && rule.audio_mode === 'any')
    if (!covered) errors.push({ code: 'coverage', ...capability })
  }
  return errors
}

export function buildVideoPricingPreview(rule: VideoPricingRuleInput) {
  const multiplier = rule.unit === 'per_output_second'
  const priceUnits = multiplier ? [5, 10] : [1, 1]
  const margin = rule.upstream_unit_cost === null ? null : rule.unit_price - rule.upstream_unit_cost
  return {
    five_second_price: rule.unit_price * priceUnits[0],
    ten_second_price: rule.unit_price * priceUnits[1],
    five_second_margin: margin === null ? null : margin * priceUnits[0],
    ten_second_margin: margin === null ? null : margin * priceUnits[1]
  }
}

function validModelMapping(value: unknown): value is Record<string, string> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) &&
    Object.entries(value).length > 0 &&
    Object.entries(value).every(([external, upstream]) =>
      external.trim() !== '' && !external.includes('*') &&
      typeof upstream === 'string' && upstream.trim() !== '')
}

export function deriveAuthoritativeVideoCapabilities(
  accounts: Array<Partial<Account>>
): VideoPricingCapability[] {
  const result = new Map<string, VideoPricingCapability>()
  for (const account of accounts) {
    if (account.platform !== 'video' || account.status !== 'active') continue
    // Kling is deliberately absent from the durable provider registry in this release.
    if (account.video_provider !== 'seedance') continue
    if (!Array.isArray(account.video_capabilities) ||
      account.video_capabilities.some(tag => typeof tag !== 'string' || !knownCapabilityTags.has(tag))) continue
    const mapping = account.extra?.model_mapping
    if (!validModelMapping(mapping)) continue
    const accountOperations = account.video_capabilities.filter(
      (tag): tag is VideoPricingOperation => operations.has(tag as VideoPricingOperation)
    )
    for (const externalModel of Object.keys(mapping)) {
      if (!externalModel.startsWith('seedance-')) continue
      for (const operation of accountOperations) {
        const capability = { external_model: externalModel, operation }
        result.set(`${externalModel}\0${operation}`, capability)
      }
    }
  }
  return [...result.values()].sort((a, b) =>
    a.external_model.localeCompare(b.external_model) || a.operation.localeCompare(b.operation))
}

export function videoPricingRulesForReplacement(
  rules: Array<VideoPricingRuleInput & { id?: number; group_id?: number }>
): VideoPricingRuleInput[] {
  return rules.map(rule => ({
    external_model: rule.external_model.trim(),
    operation: rule.operation,
    resolution: rule.resolution,
    audio_mode: rule.audio_mode,
    unit: rule.unit,
    unit_price: rule.unit_price,
    upstream_unit_cost: rule.upstream_unit_cost,
    enabled: rule.enabled
  }))
}

export function videoPricingErrorI18nKey(error: unknown): string {
  const marker = typeof error === 'object' && error !== null
    ? String((error as { code?: unknown; reason?: unknown }).reason ?? (error as { code?: unknown }).code ?? '')
    : ''
  const known: Record<string, string> = {
    VIDEO_PRICING_RULE_INVALID: 'invalidRule',
    VIDEO_PRICING_RULE_OVERLAP: 'overlap',
    VIDEO_PRICING_COVERAGE_INCOMPLETE: 'coverageIncomplete',
    IDEMPOTENCY_KEY_CONFLICT: 'conflict',
    IDEMPOTENCY_KEY_REQUIRED: 'idempotencyRequired',
    STEP_UP_TOTP_NOT_ENABLED: 'stepUpTotpRequired',
    STEP_UP_ADMIN_API_KEY_FORBIDDEN: 'stepUpSessionRequired'
  }
  return `admin.groups.videoPricing.errors.${known[marker] ?? 'generic'}`
}
