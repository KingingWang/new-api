import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import '@testing-library/jest-dom/vitest'
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { ChannelHealthSection } from '../channel-health-section'
import type { HealthSettings } from '../defaults'

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => key }),
}))

const defaultValues: HealthSettings = {
  ChannelDisableThreshold: '',
  AutomaticDisableChannelEnabled: false,
  AutomaticEnableChannelEnabled: false,
  AutomaticDisableKeywords: '',
  AutomaticDisableStatusCodes: '401',
  EmptyResponseRetryEnabled: true,
  ResponseBlacklistKeywords: 'upstream error\r\nempty response',
  'monitor_setting.auto_test_channel_enabled': false,
  'monitor_setting.auto_test_channel_minutes': 10,
  'monitor_setting.channel_test_concurrency': 32,
  'monitor_setting.channel_test_mode': 'scheduled_all',
}

let actionsContainer: HTMLDivElement | null = null

function renderSection() {
  const queryClient = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  actionsContainer = document.createElement('div')
  document.body.append(actionsContainer)

  return render(
    <QueryClientProvider client={queryClient}>
      <SettingsPageProvider actionsContainer={actionsContainer}>
        <ChannelHealthSection defaultValues={defaultValues} />
      </SettingsPageProvider>
    </QueryClientProvider>
  )
}

function lastPatchPayload() {
  const call = vi.mocked(api.patch).mock.calls.at(-1)
  return call?.[1] as { options: Record<string, string> }
}

describe('ChannelHealthSection response validation settings', () => {
  afterEach(() => {
    cleanup()
    actionsContainer?.remove()
    actionsContainer = null
    vi.restoreAllMocks()
  })

  test('loads existing option values and normalizes multiline keywords', () => {
    renderSection()

    expect(
      screen.getByRole('switch', { name: 'Retry empty responses' })
    ).toBeChecked()
    expect(screen.getByLabelText('Response blacklist keywords')).toHaveValue(
      'upstream error\nempty response'
    )
  })

  test('submits only the changed response validation switch', async () => {
    const user = userEvent.setup()
    vi.spyOn(api, 'patch').mockResolvedValue({
      data: { success: true, message: '', data: { options: {} } },
    } as never)
    renderSection()

    await user.click(
      screen.getByRole('switch', { name: 'Retry empty responses' })
    )
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))

    await waitFor(() => expect(api.patch).toHaveBeenCalledTimes(1))
    expect(lastPatchPayload()).toEqual({
      options: { EmptyResponseRetryEnabled: 'false' },
    })
  })

  test('normalizes multiline keywords before submitting the changed value', async () => {
    const user = userEvent.setup()
    vi.spyOn(api, 'patch').mockResolvedValue({
      data: { success: true, message: '', data: { options: {} } },
    } as never)
    renderSection()

    const keywords = screen.getByLabelText('Response blacklist keywords')
    await user.clear(keywords)
    fireEvent.change(keywords, {
      target: { value: 'rate limit\r\nempty response' },
    })
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))

    await waitFor(() => expect(api.patch).toHaveBeenCalledTimes(1))
    expect(lastPatchPayload()).toEqual({
      options: { ResponseBlacklistKeywords: 'rate limit\nempty response' },
    })
  })
})
