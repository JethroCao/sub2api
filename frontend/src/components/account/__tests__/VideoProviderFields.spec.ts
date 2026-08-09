import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

import VideoProviderFields from '../VideoProviderFields.vue'
import Select from '@/components/common/Select.vue'

describe('VideoProviderFields', () => {
  it('emits only the Seedance API key', async () => {
    const wrapper = mount(VideoProviderFields, {
      props: {
        provider: 'seedance',
        credentials: {},
        extra: { model_mapping: {} }
      }
    })

    await wrapper.get('[data-testid="video-api-key"]').setValue('ark')
    await wrapper.get('[data-testid="video-base-url"]').setValue('https://ark.example.com')

    expect(wrapper.emitted('update:credentials')?.at(-1)?.[0]).toEqual({
      api_key: 'ark',
      base_url: 'https://ark.example.com'
    })
    expect(wrapper.find('[data-testid="video-access-key"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="video-secret-key"]').exists()).toBe(false)
  })

  it('requires both Kling keys and emits provider credentials', async () => {
    const wrapper = mount(VideoProviderFields, {
      props: {
        provider: 'kling',
        credentials: {},
        extra: { model_mapping: {} }
      }
    })

    await wrapper.get('[data-testid="video-access-key"]').setValue('ak')
    await wrapper.get('[data-testid="video-secret-key"]').setValue('sk')

    expect(wrapper.emitted('update:credentials')?.at(-1)?.[0]).toEqual({
      access_key: 'ak',
      secret_key: 'sk'
    })
  })

  it('never renders API-returned secrets in edit mode and shows presence hints instead', () => {
    const wrapper = mount(VideoProviderFields, {
      props: {
        mode: 'edit',
        provider: 'kling',
        credentials: { access_key: 'api-returned-ak', secret_key: 'api-returned-sk' },
        credentialStatus: { has_access_key: true, has_secret_key: true },
        extra: { model_mapping: { 'kling-3.0': 'kling-v3' } }
      }
    })

    expect(wrapper.html()).not.toContain('api-returned-ak')
    expect(wrapper.html()).not.toContain('api-returned-sk')
    expect(wrapper.text()).toContain('admin.accounts.video.secretConfigured')
    expect(wrapper.get<HTMLInputElement>('[data-testid="video-access-key"]').element.value).toBe('')
    expect(wrapper.get<HTMLInputElement>('[data-testid="video-secret-key"]').element.value).toBe('')
  })

  it('clears provider-specific credentials and metadata when switching providers', async () => {
    const wrapper = mount(VideoProviderFields, {
      props: {
        provider: 'seedance',
        credentials: { api_key: 'ark', base_url: 'https://ark.example.com' },
        extra: {
          model_mapping: { 'seedance-2.0': 'ep-seedance' },
          video_disabled_capabilities: ['audio']
        }
      }
    })

    wrapper.getComponent(Select).vm.$emit('update:modelValue', 'kling')
    await wrapper.vm.$nextTick()

    expect(wrapper.emitted('update:provider')?.at(-1)?.[0]).toBe('kling')
    expect(wrapper.emitted('update:credentials')?.at(-1)?.[0]).toEqual({})
    expect(wrapper.emitted('update:extra')?.at(-1)?.[0]).toEqual({ model_mapping: {} })
  })

  it('renders capability badges and disable controls only for API-derived tags', () => {
    const dormant = mount(VideoProviderFields, {
      props: {
        mode: 'edit',
        provider: 'kling',
        credentials: {},
        extra: { model_mapping: { 'kling-3.0': 'kling-v3' } },
        capabilityTags: []
      }
    })
    expect(dormant.find('[data-testid="video-capability-generation"]').exists()).toBe(false)

    const derived = mount(VideoProviderFields, {
      props: {
        mode: 'edit',
        provider: 'seedance',
        credentials: {},
        extra: { model_mapping: { 'seedance-2.0': 'ep-seedance' } },
        capabilityTags: ['generation', 'audio']
      }
    })
    expect(derived.find('[data-testid="video-capability-generation"]').exists()).toBe(true)
    expect(derived.find('[data-testid="video-disable-generation"]').exists()).toBe(true)
    expect(derived.find('[data-testid="video-capability-edit"]').exists()).toBe(false)
  })

  it('lets an existing disabled capability be re-enabled without claiming it as effective', async () => {
    const wrapper = mount(VideoProviderFields, {
      props: {
        mode: 'edit',
        provider: 'seedance',
        credentials: {},
        extra: {
          model_mapping: { 'seedance-2.0': 'ep-seedance' },
          video_disabled_capabilities: ['audio']
        },
        capabilityTags: ['generation']
      }
    })

    expect(wrapper.find('[data-testid="video-capability-audio"]').exists()).toBe(false)
    const control = wrapper.get<HTMLInputElement>('[data-testid="video-disable-audio"]')
    expect(control.element.checked).toBe(true)
    await control.setValue(false)
    expect(wrapper.emitted('update:extra')?.at(-1)?.[0]).toEqual({
      model_mapping: { 'seedance-2.0': 'ep-seedance' }
    })
  })

  it('drops stale Seedance capability tags after switching to Kling', async () => {
    const wrapper = mount(VideoProviderFields, {
      props: {
        mode: 'edit',
        provider: 'seedance',
        credentials: {},
        extra: { model_mapping: { 'seedance-2.0': 'ep-seedance' } },
        capabilityTags: ['generation', 'audio']
      }
    })

    expect(wrapper.find('[data-testid="video-capability-generation"]').exists()).toBe(true)
    wrapper.getComponent(Select).vm.$emit('update:modelValue', 'kling')
    await wrapper.vm.$nextTick()
    expect(wrapper.find('[data-testid="video-capability-generation"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="video-disable-generation"]').exists()).toBe(false)
  })

  it('emits an explicit empty base URL only when edit mode clears an existing value', async () => {
    const edit = mount(VideoProviderFields, {
      props: {
        mode: 'edit',
        provider: 'seedance',
        credentials: { base_url: 'https://ark.example.com' },
        extra: { model_mapping: { 'seedance-2.0': 'ep-seedance' } }
      }
    })
    await edit.get('[data-testid="video-base-url"]').setValue('')
    expect(edit.emitted('update:credentials')?.at(-1)?.[0]).toEqual({ base_url: '' })

    const create = mount(VideoProviderFields, {
      props: {
        mode: 'create',
        provider: 'seedance',
        credentials: {},
        extra: { model_mapping: {} }
      }
    })
    await create.get('[data-testid="video-base-url"]').setValue('')
    expect(create.emitted('update:credentials')?.at(-1)?.[0]).toEqual({})
  })
})
