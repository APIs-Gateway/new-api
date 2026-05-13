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
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { CheckSquare, RefreshCcw, Save, Search, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { getEnabledModels } from '@/features/channels/api'
import { useQuery } from '@tanstack/react-query'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

const SESSION_ARCHIVE_ENABLED_MODELS_KEY =
  'session_archive_setting.enabled_models'
const SESSION_ARCHIVE_MODEL_ALIASES_KEY =
  'session_archive_setting.model_aliases'

type SessionArchiveSettingsCardProps = {
  defaultValues: {
    'session_archive_setting.enabled_models': string
    'session_archive_setting.model_aliases': string
  }
}

function normalizeModelNames(values: string[]) {
  const seen = new Set<string>()
  const normalized: string[] = []

  for (const value of values) {
    const trimmed = value.trim()
    if (!trimmed || seen.has(trimmed)) {
      continue
    }
    seen.add(trimmed)
    normalized.push(trimmed)
  }

  normalized.sort((a, b) => a.localeCompare(b))
  return normalized
}

function parseModelNames(raw: string) {
  const trimmed = (raw ?? '').trim()
  if (!trimmed) {
    return []
  }

  try {
    const parsed = JSON.parse(trimmed)
    if (!Array.isArray(parsed)) {
      return []
    }
    return normalizeModelNames(
      parsed.filter((value): value is string => typeof value === 'string')
    )
  } catch {
    return []
  }
}

function normalizeModelAliases(values: Record<string, string>) {
  const normalized: Record<string, string> = {}

  for (const [source, target] of Object.entries(values)) {
    const sourceName = source.trim()
    const targetName = target.trim()
    if (!sourceName || !targetName || sourceName === targetName) {
      continue
    }
    normalized[sourceName] = targetName
  }

  return Object.fromEntries(
    Object.entries(normalized).sort(([a], [b]) => a.localeCompare(b))
  )
}

function parseModelAliases(raw: string) {
  const trimmed = (raw ?? '').trim()
  if (!trimmed) {
    return {}
  }

  try {
    const parsed = JSON.parse(trimmed)
    if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') {
      return {}
    }
    const aliases: Record<string, string> = {}
    for (const [key, value] of Object.entries(parsed)) {
      if (typeof value === 'string') {
        aliases[key] = value
      }
    }
    return normalizeModelAliases(aliases)
  } catch {
    return {}
  }
}

export function SessionArchiveSettingsCard({
  defaultValues,
}: SessionArchiveSettingsCardProps) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const [search, setSearch] = useState('')
  const enabledModelsQuery = useQuery({
    queryKey: ['session-archive-enabled-models'],
    queryFn: async () => {
      const response = await getEnabledModels()
      if (!response.success) {
        throw new Error(response.message || 'Failed to load')
      }
      return response.data ?? []
    },
    staleTime: 60 * 1000,
  })
  const {
    data: enabledModels,
    isLoading: isModelsLoading,
    error: modelsError,
    refetch: refetchModels,
  } = enabledModelsQuery
  const defaultModelNames = useMemo(
    () => parseModelNames(defaultValues[SESSION_ARCHIVE_ENABLED_MODELS_KEY]),
    [defaultValues[SESSION_ARCHIVE_ENABLED_MODELS_KEY]]
  )
  const defaultModelAliases = useMemo(
    () => parseModelAliases(defaultValues[SESSION_ARCHIVE_MODEL_ALIASES_KEY]),
    [defaultValues[SESSION_ARCHIVE_MODEL_ALIASES_KEY]]
  )
  const [selectedModelNames, setSelectedModelNames] = useState(() =>
    defaultModelNames
  )
  const [modelAliases, setModelAliases] = useState(() => defaultModelAliases)

  useEffect(() => {
    setSelectedModelNames(defaultModelNames)
  }, [defaultModelNames])

  useEffect(() => {
    setModelAliases(defaultModelAliases)
  }, [defaultModelAliases])

  const availableModelNames = useMemo(
    () => normalizeModelNames(enabledModels ?? []),
    [enabledModels]
  )
  const allSelectableModelNames = useMemo(
    () => normalizeModelNames([...availableModelNames, ...selectedModelNames]),
    [availableModelNames, selectedModelNames]
  )
  const normalizedSelectedModelNames = useMemo(
    () => normalizeModelNames(selectedModelNames),
    [selectedModelNames]
  )
  const normalizedModelAliases = useMemo(() => {
    const selected = new Set(normalizedSelectedModelNames)
    const selectedAliases: Record<string, string> = {}
    for (const [source, target] of Object.entries(modelAliases)) {
      if (selected.has(source)) {
        selectedAliases[source] = target
      }
    }
    return normalizeModelAliases(selectedAliases)
  }, [modelAliases, normalizedSelectedModelNames])
  const normalizedDefaultModelAliases = useMemo(() => {
    const selected = new Set(defaultModelNames)
    const selectedAliases: Record<string, string> = {}
    for (const [source, target] of Object.entries(defaultModelAliases)) {
      if (selected.has(source)) {
        selectedAliases[source] = target
      }
    }
    return normalizeModelAliases(selectedAliases)
  }, [defaultModelAliases, defaultModelNames])
  const defaultSerialized = useMemo(
    () =>
      JSON.stringify({
        models: defaultModelNames,
        aliases: normalizedDefaultModelAliases,
      }),
    [defaultModelNames, normalizedDefaultModelAliases]
  )
  const currentSerialized = useMemo(
    () =>
      JSON.stringify({
        models: normalizedSelectedModelNames,
        aliases: normalizedModelAliases,
      }),
    [normalizedModelAliases, normalizedSelectedModelNames]
  )
  const isDirty = currentSerialized !== defaultSerialized

  const filteredModelNames = useMemo(() => {
    const query = search.trim().toLowerCase()
    if (!query) {
      return allSelectableModelNames
    }
    return allSelectableModelNames.filter((modelName) =>
      modelName.toLowerCase().includes(query)
    )
  }, [allSelectableModelNames, search])

  const handleToggleModel = (modelName: string, checked: boolean) => {
    setSelectedModelNames((current) => {
      if (checked) {
        return normalizeModelNames([...current, modelName])
      }
      return normalizeModelNames(current.filter((item) => item !== modelName))
    })
  }

  const handleAliasChange = (modelName: string, alias: string) => {
    setModelAliases((current) => ({
      ...current,
      [modelName]: alias,
    }))
  }

  const handleSelectAll = () => {
    setSelectedModelNames(allSelectableModelNames)
  }

  const handleClearAll = () => {
    setSelectedModelNames([])
  }

  const handleSave = async () => {
    try {
      const enabledModelsResult = await updateOption.mutateAsync({
        key: SESSION_ARCHIVE_ENABLED_MODELS_KEY,
        value: JSON.stringify(normalizedSelectedModelNames),
      })
      if (!enabledModelsResult.success) {
        return
      }
      await updateOption.mutateAsync({
        key: SESSION_ARCHIVE_MODEL_ALIASES_KEY,
        value: JSON.stringify(normalizedModelAliases),
      })
    } catch {
      // The mutation handler already surfaces the error via toast.
    }
  }

  return (
    <SettingsSection
      title={t('Session Archive')}
      description={t(
        'Choose which models have full request and response context written to JSONL.'
      )}
    >
      <div className='space-y-4'>
        <div className='flex flex-wrap items-center gap-2'>
          <div className='relative min-w-0 flex-1'>
            <Search className='text-muted-foreground pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2' />
            <Input
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder={t('Search')}
              aria-label={t('Search')}
              className='pl-9'
            />
          </div>
          <Badge variant='secondary'>
            {t('{{n}} model(s) selected', {
              n: normalizedSelectedModelNames.length,
            })}
          </Badge>
          <div className='flex flex-wrap gap-2'>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={handleSelectAll}
              disabled={allSelectableModelNames.length === 0}
            >
              <CheckSquare className='mr-2 size-4' />
              {t('Select all')}
            </Button>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={handleClearAll}
              disabled={normalizedSelectedModelNames.length === 0}
            >
              <X className='mr-2 size-4' />
              {t('Clear all')}
            </Button>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={() => void refetchModels()}
              disabled={isModelsLoading}
            >
              <RefreshCcw
                className={
                  isModelsLoading
                    ? 'mr-2 size-4 animate-spin'
                    : 'mr-2 size-4'
                }
              />
              {t('Refresh')}
            </Button>
          </div>
        </div>

        {modelsError ? (
          <div className='text-destructive text-sm'>{t('Failed to load')}</div>
        ) : null}

        <div className='border-border divide-y rounded-lg border'>
          {isModelsLoading && allSelectableModelNames.length === 0 ? (
            <div className='text-muted-foreground py-8 text-center text-sm'>
              {t('Loading...')}
            </div>
          ) : filteredModelNames.length === 0 ? (
            <div className='text-muted-foreground py-8 text-center text-sm'>
              {allSelectableModelNames.length === 0
                ? t('No models available')
                : t('No matching results')}
            </div>
          ) : (
            filteredModelNames.map((modelName) => {
              const checked = normalizedSelectedModelNames.includes(modelName)
              return (
                <div
                  key={modelName}
                  className='flex flex-col gap-2 px-3 py-2 hover:bg-muted/50 sm:flex-row sm:items-center sm:gap-3'
                >
                  <div className='flex min-w-0 flex-1 items-center gap-3'>
                    <Checkbox
                      aria-label={modelName}
                      checked={checked}
                      onCheckedChange={(value) =>
                        handleToggleModel(modelName, value === true)
                      }
                    />
                    <button
                      type='button'
                      onClick={() => handleToggleModel(modelName, !checked)}
                      className='min-w-0 flex-1 cursor-pointer break-all border-0 bg-transparent p-0 text-left text-sm text-inherit'
                    >
                      {modelName}
                    </button>
                  </div>
                  {checked ? (
                    <Input
                      value={modelAliases[modelName] ?? ''}
                      onChange={(event) =>
                        handleAliasChange(modelName, event.target.value)
                      }
                      placeholder={t('Archive as')}
                      aria-label={t('Archive as {{model}}', {
                        model: modelName,
                      })}
                      className='h-8 w-full min-w-0 sm:w-64 sm:flex-none'
                    />
                  ) : null}
                </div>
              )
            })
          )}
        </div>

        <div className='flex justify-end'>
          <Button
            type='button'
            onClick={() => void handleSave()}
            disabled={!isDirty || updateOption.isPending}
          >
            <Save className='mr-2 size-4' />
            {t('Save Changes')}
          </Button>
        </div>
      </div>
    </SettingsSection>
  )
}
