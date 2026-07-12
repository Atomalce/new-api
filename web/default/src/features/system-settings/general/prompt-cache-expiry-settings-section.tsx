/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

// Bounds mirror the backend clamp in
// setting/operation_setting/prompt_cache_expiry_setting.go.
const MIN_CYCLE_SECONDS = 10
const MAX_CYCLE_SECONDS = 86400

const schema = z.object({
  enabled: z.boolean(),
  cycleSeconds: z.coerce
    .number()
    .int()
    .min(MIN_CYCLE_SECONDS)
    .max(MAX_CYCLE_SECONDS),
})

type Values = z.infer<typeof schema>

export function PromptCacheExpirySettingsSection({
  defaultValues,
}: {
  defaultValues: {
    enabled: boolean
    cycleSeconds: number
  }
}) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()

  const form = useForm<Values>({
    resolver: zodResolver(schema) as unknown as Resolver<Values>,
    defaultValues: {
      enabled: defaultValues.enabled,
      cycleSeconds: defaultValues.cycleSeconds,
    },
  })

  const { isDirty, isSubmitting } = form.formState
  const enabled = form.watch('enabled')

  async function onSubmit(values: Values) {
    const updates: Array<{ key: string; value: string }> = []

    if (values.enabled !== defaultValues.enabled) {
      updates.push({
        key: 'prompt_cache_expiry_setting.enabled',
        value: String(values.enabled),
      })
    }

    if (values.cycleSeconds !== defaultValues.cycleSeconds) {
      updates.push({
        key: 'prompt_cache_expiry_setting.cycle_seconds',
        value: String(values.cycleSeconds),
      })
    }

    if (updates.length === 0) {
      toast.info(t('No changes to save'))
      return
    }

    for (const update of updates) {
      await updateOption.mutateAsync(update)
    }

    form.reset(values)
  }

  return (
    <SettingsSection title={t('Prompt Cache Expiry Billing')}>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending || isSubmitting}
            isSaveDisabled={!isDirty}
            saveLabel='Save prompt cache expiry settings'
          />
          <FormField
            control={form.control}
            name='enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>
                    {t('Enable Codex prompt cache expiry billing')}
                  </FormLabel>
                  <FormDescription>
                    {t(
                      'Each cycle, the first Codex /v1/responses request of the same cache lineage is billed with its cached input at the full input price, and the usage returned to the client is adjusted to match. Requires Redis and a shared SESSION_SECRET/CRYPTO_SECRET on every node; otherwise the policy stays inactive even when enabled.'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    disabled={updateOption.isPending || isSubmitting}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />

          {enabled && (
            <div className='grid gap-6 sm:grid-cols-2'>
              <FormField
                control={form.control}
                name='cycleSeconds'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Cache expiry cycle (seconds)')}</FormLabel>
                    <FormControl>
                      <Input
                        type='number'
                        min={MIN_CYCLE_SECONDS}
                        max={MAX_CYCLE_SECONDS}
                        placeholder={t('60')}
                        {...field}
                      />
                    </FormControl>
                    <FormDescription>
                      {t(
                        'Cycle length in seconds (10-86400). Changes apply to newly opened cycles; running cycles keep their original expiry.'
                      )}
                    </FormDescription>
                    <FormMessage />
                  </FormItem>
                )}
              />
            </div>
          )}
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
