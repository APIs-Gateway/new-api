/*
Copyright (C) 2025 QuantumNous

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

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Banner, Button, Card, Checkbox, Input, Spin, Tag } from '@douyinfe/semi-ui';
import {
  CheckSquare,
  RefreshCcw,
  Save,
  Search,
  X,
} from 'lucide-react';
import { API, showError, showSuccess, showWarning } from '../../../helpers';
import { useTranslation } from 'react-i18next';

const SESSION_ARCHIVE_ENABLED_MODELS_KEY =
  'session_archive_setting.enabled_models';
const MODEL_LIST_PAGE_SIZE = 200;

function normalizeModelNames(values = []) {
  const seen = new Set();
  const normalized = [];

  for (const value of values) {
    const trimmed = String(value ?? '').trim();
    if (!trimmed || seen.has(trimmed)) {
      continue;
    }
    seen.add(trimmed);
    normalized.push(trimmed);
  }

  normalized.sort((a, b) => a.localeCompare(b));
  return normalized;
}

function parseModelNames(raw) {
  const text = String(raw ?? '').trim();
  if (!text) {
    return [];
  }

  try {
    const parsed = JSON.parse(text);
    if (!Array.isArray(parsed)) {
      return [];
    }
    return normalizeModelNames(
      parsed.filter((value) => typeof value === 'string'),
    );
  } catch {
    return [];
  }
}

async function fetchAllModelNames() {
  const modelNames = [];
  let page = 1;

  for (;;) {
    const res = await API.get(
      `/api/models/?p=${page}&page_size=${MODEL_LIST_PAGE_SIZE}`,
    );
    const { success, message, data } = res.data;
    if (!success) {
      throw new Error(message || 'Failed to retrieve model list');
    }

    const items = Array.isArray(data?.items)
      ? data.items
      : Array.isArray(data)
        ? data
        : [];

    items.forEach((item) => {
      if (item?.model_name) {
        modelNames.push(item.model_name);
      }
    });

    const total = Number(data?.total ?? modelNames.length);
    if (modelNames.length >= total || items.length < MODEL_LIST_PAGE_SIZE) {
      break;
    }

    page += 1;
  }

  return normalizeModelNames(modelNames);
}

async function fetchCurrentEnabledModels() {
  const res = await API.get('/api/option/');
  const { success, message, data } = res.data;
  if (!success) {
    throw new Error(message || 'Failed to load options');
  }

  const option = Array.isArray(data)
    ? data.find((item) => item.key === SESSION_ARCHIVE_ENABLED_MODELS_KEY)
    : null;

  return option?.value ?? '[]';
}

const SessionArchiveSettings = () => {
  const { t } = useTranslation();
  const [availableModelNames, setAvailableModelNames] = useState([]);
  const [selectedModelNames, setSelectedModelNames] = useState([]);
  const [savedModelNames, setSavedModelNames] = useState([]);
  const [searchText, setSearchText] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);

  const loadData = useCallback(async () => {
    setLoading(true);
    try {
      const [modelNames, rawEnabledModels] = await Promise.all([
        fetchAllModelNames(),
        fetchCurrentEnabledModels(),
      ]);
      const parsedSelected = parseModelNames(rawEnabledModels);
      setAvailableModelNames(modelNames);
      setSelectedModelNames(parsedSelected);
      setSavedModelNames(parsedSelected);
    } catch (error) {
      console.error(error);
      showError(error?.message || t('加载失败'));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const normalizedSelectedModelNames = useMemo(
    () => normalizeModelNames(selectedModelNames),
    [selectedModelNames],
  );
  const normalizedSavedModelNames = useMemo(
    () => normalizeModelNames(savedModelNames),
    [savedModelNames],
  );
  const allSelectableModelNames = useMemo(
    () =>
      normalizeModelNames([
        ...availableModelNames,
        ...normalizedSelectedModelNames,
      ]),
    [availableModelNames, normalizedSelectedModelNames],
  );
  const selectedModelNameSet = useMemo(
    () => new Set(normalizedSelectedModelNames),
    [normalizedSelectedModelNames],
  );
  const filteredModelNames = useMemo(() => {
    const keyword = searchText.trim().toLowerCase();
    if (!keyword) {
      return allSelectableModelNames;
    }
    return allSelectableModelNames.filter((modelName) =>
      modelName.toLowerCase().includes(keyword),
    );
  }, [allSelectableModelNames, searchText]);
  const isDirty =
    JSON.stringify(normalizedSelectedModelNames) !==
    JSON.stringify(normalizedSavedModelNames);

  const handleToggleModel = (modelName, checked) => {
    setSelectedModelNames((current) => {
      if (checked) {
        return normalizeModelNames([...current, modelName]);
      }
      return normalizeModelNames(current.filter((item) => item !== modelName));
    });
  };

  const handleSelectAll = () => {
    setSelectedModelNames(allSelectableModelNames);
  };

  const handleClearAll = () => {
    setSelectedModelNames([]);
  };

  const handleSave = async () => {
    if (!isDirty) {
      return showWarning(t('你似乎并没有修改什么'));
    }

    setSaving(true);
    try {
      const res = await API.put('/api/option/', {
        key: SESSION_ARCHIVE_ENABLED_MODELS_KEY,
        value: JSON.stringify(normalizedSelectedModelNames),
      });
      const { success, message } = res.data;
      if (success) {
        setSavedModelNames(normalizedSelectedModelNames);
        showSuccess(t('保存成功'));
      } else {
        showError(message || t('保存失败，请重试'));
      }
    } catch (error) {
      showError(error?.response?.data?.message || t('保存失败，请重试'));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card
      style={{ marginBottom: '10px' }}
      bodyStyle={{ padding: '16px' }}
      title={t('会话归档设置')}
    >
      <div className='flex flex-col gap-4'>
        <Banner
          type='info'
          description={t('选择需要记录完整请求和响应上下文的模型。')}
          style={{ marginBottom: 0 }}
        />

        <div className='flex flex-wrap items-center gap-2'>
          <div className='relative min-w-0 flex-1'>
            <Input
              prefix={<Search size={16} />}
              value={searchText}
              onChange={(value) => setSearchText(value)}
              placeholder={t('搜索模型')}
              aria-label={t('搜索模型')}
              showClear
            />
          </div>
          <Tag color='blue'>
            {t('已选择 {{count}} 个模型', {
              count: normalizedSelectedModelNames.length,
            })}
          </Tag>
          <div className='flex flex-wrap gap-2'>
            <Button
              size='small'
              type='tertiary'
              icon={<CheckSquare size={16} />}
              onClick={handleSelectAll}
              disabled={allSelectableModelNames.length === 0}
            >
              {t('全选')}
            </Button>
            <Button
              size='small'
              type='tertiary'
              icon={<X size={16} />}
              onClick={handleClearAll}
              disabled={normalizedSelectedModelNames.length === 0}
            >
              {t('清空')}
            </Button>
            <Button
              size='small'
              type='tertiary'
              icon={<RefreshCcw size={16} />}
              onClick={() => void loadData()}
              disabled={loading}
            >
              {t('刷新')}
            </Button>
            <Button
              size='small'
              type='primary'
              icon={<Save size={16} />}
              onClick={() => void handleSave()}
              disabled={!isDirty || saving}
              loading={saving}
            >
              {t('保存设置')}
            </Button>
          </div>
        </div>

        <Spin spinning={loading}>
          <div
            className='max-h-[420px] overflow-y-auto rounded-lg'
            style={{ border: '1px solid var(--semi-color-border)' }}
          >
            {filteredModelNames.length === 0 ? (
              <div
                className='py-8 text-center text-sm'
                style={{ color: 'var(--semi-color-text-2)' }}
              >
                {allSelectableModelNames.length === 0
                  ? t('没有可用模型')
                  : t('搜索无结果')}
              </div>
            ) : (
              filteredModelNames.map((modelName) => {
                const checked = selectedModelNameSet.has(modelName);
                return (
                  <label
                    key={modelName}
                    className='flex cursor-pointer items-center gap-3 border-b px-3 py-2 last:border-b-0 hover:bg-[var(--semi-color-fill-0)]'
                  >
                    <Checkbox
                      checked={checked}
                      onChange={(event) =>
                        handleToggleModel(modelName, event.target.checked)
                      }
                    />
                    <span className='min-w-0 flex-1 break-all text-sm'>
                      {modelName}
                    </span>
                  </label>
                );
              })
            )}
          </div>
        </Spin>
      </div>
    </Card>
  );
};

export default SessionArchiveSettings;
