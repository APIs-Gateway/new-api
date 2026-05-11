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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { CheckSquare, RefreshCcw, Save, Search, X } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { getModels } from '@/features/models/api'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

const SESSION_ARCHIVE_ENABLED_MODELS_KEY =
  'session_archive_setting.enabled_models'

const MODEL_LIST_PAGE_SIZE = 100

type SessionArchiveSettingsCardProps = {
  defaultValues: {
    'session_archive_setting.enabled_models': string
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

async function fetchAllModelNames() {
  const modelNames: string[] = []
  let page = 1

  for (;;) {
    const response = await getModels({
      p: page,
      page_size: MODEL_LIST_PAGE_SIZE,
    })

    if (!response.success || !response.data) {
      throw new Error(response.message || 'Failed to load')
    }

    const batch =
      response.data.items?.reduce<string[]>((acc, item) => {
        if (item.model_name) {
          acc.push(item.model_name)
        }
        return acc
      }, []) ?? []
    modelNames.push(...batch)

    const total = response.data.total ?? modelNames.length
    if (modelNames.length >= total || batch.length < MODEL_LIST_PAGE_SIZE) {
      break
    }

    page += 1
  }

  return normalizeModelNames(modelNames)
}

export function SessionArchiveSettingsCard({
  defaultValues,
}: SessionArchiveSettingsCardProps) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const [search, setSearch] = useState('')
  const defaultModelNames = useMemo(
    () => parseModelNames(defaultValues[SESSION_ARCHIVE_ENABLED_MODELS_KEY]),
    [defaultValues[SESSION_ARCHIVE_ENABLED_MODELS_KEY]]
  )
  const [selectedModelNames, setSelectedModelNames] = useState(() =>
    defaultModelNames
  )

  useEffect(() => {
    setSelectedModelNames(defaultModelNames)
  }, [defaultModelNames])

  const modelsQuery = useQuery({
    queryKey: ['system-settings', 'session-archive-models'],
    queryFn: fetchAllModelNames,
    staleTime: 5 * 60 * 1000,
  })

  const availableModelNames = modelsQuery.data ?? []
  const allSelectableModelNames = useMemo(
    () => normalizeModelNames([...availableModelNames, ...selectedModelNames]),
    [availableModelNames, selectedModelNames]
  )
  const normalizedSelectedModelNames = useMemo(
    () => normalizeModelNames(selectedModelNames),
    [selectedModelNames]
  )
  const defaultSerialized = useMemo(
    () => JSON.stringify(defaultModelNames),
    [defaultModelNames]
  )
  const currentSerialized = useMemo(
    () => JSON.stringify(normalizedSelectedModelNames),
    [normalizedSelectedModelNames]
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

  const handleSelectAll = () => {
    setSelectedModelNames(allSelectableModelNames)
  }

  const handleClearAll = () => {
    setSelectedModelNames([])
  }

  const handleSave = async () => {
    try {
      await updateOption.mutateAsync({
        key: SESSION_ARCHIVE_ENABLED_MODELS_KEY,
        value: JSON.stringify(normalizedSelectedModelNames),
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
              onClick={() => void modelsQuery.refetch()}
              disabled={modelsQuery.isFetching}
            >
              <RefreshCcw
                className={
                  modelsQuery.isFetching
                    ? 'mr-2 size-4 animate-spin'
                    : 'mr-2 size-4'
                }
              />
              {t('Refresh')}
            </Button>
          </div>
        </div>

        {modelsQuery.isError ? (
          <div className='text-destructive text-sm'>{t('Failed to load')}</div>
        ) : null}

        <div className='border-border divide-y rounded-lg border'>
          {modelsQuery.isLoading && allSelectableModelNames.length === 0 ? (
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
                <label
                  key={modelName}
                  className='flex cursor-pointer items-center gap-3 px-3 py-2 hover:bg-muted/50'
                >
                  <Checkbox
                    checked={checked}
                    onCheckedChange={(value) =>
                      handleToggleModel(modelName, value === true)
                    }
                  />
                  <span className='min-w-0 flex-1 break-all text-sm'>
                    {modelName}
                  </span>
                </label>
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
