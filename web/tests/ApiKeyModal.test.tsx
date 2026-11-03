import React from 'react';
import { AxiosError } from 'axios';
import type { AxiosAdapter, AxiosResponse, InternalAxiosRequestConfig } from 'axios';
import { ConfigProvider, message } from 'antd';
import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Mock } from 'vitest';
import { ApiKeyModal } from '../src/presentation/components/ApiKeyModal';
import {
  getApiKey,
  httpClient,
  onAuthFailure,
  onCredentialChange,
  setApiKey,
} from '../src/infrastructure/http/client';

// These deliberately invalid credentials belong only to this in-memory gateway.
const CREATED_KEY = 'synthetic-created-key-for-tests-only';
const OTHER_KEY = 'synthetic-other-key-for-tests-only';
const STORAGE_KEY = 'oaiprism_api_key';

interface KeyItem {
  key: string;
  name: string;
  created_at: string;
}

interface RecordedRequest {
  method: string;
  url: string;
  authorization: string;
  storedKey: string | null;
}

const keyItem = (key: string, name: string): KeyItem => ({
  key,
  name,
  created_at: '2026-10-10T00:00:00Z',
});

/**
 * Models the server's transition from local bootstrap to authenticated requests.
 * Requests still pass through the real Axios request/response interceptors.
 * No test starts a server, contacts an upstream, or reads production credentials.
 */
class Gateway {
  keys: KeyItem[] = [];
  privilegedKeys = new Set<string>();
  remote = false;
  requests: RecordedRequest[] = [];
  rejectCreate = false;
  rejectNextList = false;
  prependOtherKeyOnCreate = false;

  readonly adapter: AxiosAdapter = async (config) => {
    const method = config.method?.toUpperCase() ?? 'GET';
    const url = config.url ?? '';
    const authorization = String(config.headers.get('Authorization') ?? '');
    this.requests.push({ method, url, authorization, storedKey: localStorage.getItem(STORAGE_KEY) });

    const key = authorization.replace(/^Bearer /, '');
    const privileged = this.privilegedKeys.has(key);
    const valid = privileged || this.keys.some((item) => item.key === key);
    const anyKeys = this.keys.length > 0 || this.privilegedKeys.size > 0;
    if (!valid && (anyKeys || this.remote)) {
      this.fail(config, 401, { error: { code: 'invalid_api_key', message: 'Synthetic invalid credential' } });
    }
    if (this.remote && url.startsWith('/admin/') && !privileged) {
      this.fail(config, 403, { error: { code: 'admin_forbidden', message: 'Synthetic admin credential required' } });
    }

    if (method === 'GET' && url === '/v1/models') {
      return this.respond(config, { data: [] });
    }
    if (method === 'GET' && url === '/admin/apikeys') {
      if (this.rejectNextList) {
        this.rejectNextList = false;
        this.fail(config, 503, { error: 'Synthetic list unavailable' });
      }
      return this.respond(config, [...this.keys]);
    }
    if (method === 'POST' && url === '/admin/apikeys') {
      if (this.rejectCreate) this.fail(config, 500, { error: 'Synthetic create failed' });
      const body = JSON.parse(config.data);
      const created = keyItem(CREATED_KEY, body.name);
      this.keys.unshift(created);
      // Deliberately return a list order unrelated to the creation response.
      if (this.prependOtherKeyOnCreate) this.keys.unshift(keyItem(OTHER_KEY, 'Another client'));
      return this.respond(config, created);
    }
    if (method === 'DELETE' && url.startsWith('/admin/apikeys/')) {
      const deleted = url.slice('/admin/apikeys/'.length);
      this.keys = this.keys.filter((item) => item.key !== deleted);
      return this.respond(config, { ok: true });
    }
    throw new Error(`Unexpected test request: ${method} ${url}`);
  };

  private respond(config: InternalAxiosRequestConfig, data: unknown, status = 200): AxiosResponse {
    return { config, data, status, statusText: String(status), headers: {} };
  }

  private fail(config: InternalAxiosRequestConfig, status: number, data: unknown): never {
    throw new AxiosError('Synthetic gateway error', AxiosError.ERR_BAD_RESPONSE, config, undefined,
      this.respond(config, data, status));
  }

  afterCreate(): RecordedRequest[] {
    const index = this.requests.findIndex((request) => request.method === 'POST');
    return index < 0 ? [] : this.requests.slice(index + 1);
  }
}

const modal = (open = true) => (
  <ConfigProvider theme={{ token: { motion: false } }}>
    <ApiKeyModal open={open} onClose={() => {}} />
  </ConfigProvider>
);

describe('API key creation and credential persistence', () => {
  let gateway: Gateway;
  let authFailure: Mock<(message: string) => void>;
  let unsubscribeAuth: () => void;
  const originalAdapter = httpClient.defaults.adapter;

  beforeEach(() => {
    localStorage.clear();
    gateway = new Gateway();
    httpClient.defaults.adapter = gateway.adapter;
    authFailure = vi.fn();
    unsubscribeAuth = onAuthFailure(authFailure);
    vi.spyOn(message, 'success').mockImplementation(() => undefined as never);
    vi.spyOn(message, 'error').mockImplementation(() => undefined as never);
  });

  afterEach(() => {
    unsubscribeAuth();
    httpClient.defaults.adapter = originalAdapter;
  });

  async function openModal() {
    const view = render(modal());
    await waitFor(() => expect(gateway.requests.filter((r) => r.method === 'GET')).toHaveLength(2));
    await waitFor(() => expect(document.querySelector('.ant-spin-spinning')).toBeNull());
    return view;
  }

  async function createKey() {
    await userEvent.click(screen.getByRole('button', { name: /生成新 Key/ }));
    await waitFor(() => expect(gateway.afterCreate().some((r) => r.url === '/admin/apikeys')).toBe(true));
  }

  it('persists the first creation response before refreshing or notifying credential listeners', async () => {
    await openModal();
    const listenerRequests: Promise<unknown>[] = [];
    const observedStorage: Array<string | null> = [];
    const unsubscribe = onCredentialChange(() => {
      observedStorage.push(localStorage.getItem(STORAGE_KEY));
      listenerRequests.push(httpClient.get('/v1/models'));
    });
    try {
      await createKey();
      await act(async () => { await Promise.all(listenerRequests); });
      expect(gateway.requests.find((r) => r.method === 'POST')?.authorization).toBe('');
      expect(localStorage.getItem(STORAGE_KEY)).toBe(CREATED_KEY);
      expect(observedStorage).toEqual([CREATED_KEY]);
      expect(gateway.afterCreate()).toHaveLength(2);
      for (const request of gateway.afterCreate()) {
        expect(request.authorization).toBe(`Bearer ${CREATED_KEY}`);
        expect(request.storedKey).toBe(CREATED_KEY);
      }
      expect(await screen.findByText('新访问密钥')).toBeTruthy();
      expect(authFailure).not.toHaveBeenCalled();
      expect(screen.queryByText(/无法读取密钥列表/)).toBeNull();
    } finally {
      unsubscribe();
    }
  });

  it('uses the POST response, independent of the refreshed list order', async () => {
    gateway.prependOtherKeyOnCreate = true;
    await openModal();
    await createKey();
    await screen.findByText('Another client');
    expect(gateway.keys[0].key).toBe(OTHER_KEY);
    expect(getApiKey()).toBe(CREATED_KEY);
    expect(gateway.afterCreate()[0].authorization).toBe(`Bearer ${CREATED_KEY}`);
    expect(authFailure).not.toHaveBeenCalled();
  });

  it('keeps the created credential if the list refresh fails and can refresh on reopening', async () => {
    const view = await openModal();
    gateway.rejectNextList = true;
    await createKey();
    expect(await screen.findByText(/Synthetic list unavailable/)).toBeTruthy();
    expect(localStorage.getItem(STORAGE_KEY)).toBe(CREATED_KEY);
    expect(message.error).not.toHaveBeenCalled();

    view.rerender(modal(false));
    view.rerender(modal(true));
    expect(await screen.findByText('新访问密钥')).toBeTruthy();
    expect(screen.queryByText(/无法读取密钥列表/)).toBeNull();
    expect(gateway.requests.at(-1)?.authorization).toBe(`Bearer ${CREATED_KEY}`);
    expect(authFailure).not.toHaveBeenCalled();
  });

  it('restores the created credential in a fresh HTTP client after a page reload', async () => {
    const view = await openModal();
    await createKey();
    await screen.findByText('新访问密钥');
    view.unmount();

    // Preserve only browser storage; re-evaluate client.ts to discard module state.
    vi.resetModules();
    const reloaded = await import('../src/infrastructure/http/client');
    expect(reloaded.httpClient).not.toBe(httpClient);
    reloaded.httpClient.defaults.adapter = gateway.adapter;
    const reloadedFailure = vi.fn();
    const unsubscribe = reloaded.onAuthFailure(reloadedFailure);
    try {
      const start = gateway.requests.length;
      await reloaded.httpClient.get('/admin/apikeys');
      await reloaded.httpClient.get('/v1/models');
      expect(reloaded.getApiKey()).toBe(CREATED_KEY);
      expect(gateway.requests.slice(start).map((r) => r.authorization))
        .toEqual([`Bearer ${CREATED_KEY}`, `Bearer ${CREATED_KEY}`]);
      expect(reloadedFailure).not.toHaveBeenCalled();
    } finally {
      unsubscribe();
    }
  });

  it('rotates to the new response key, then deletes the old key without losing access', async () => {
    gateway.keys.push(keyItem(OTHER_KEY, 'Previous client'));
    setApiKey(OTHER_KEY);
    const view = await openModal();
    await userEvent.type(screen.getByPlaceholderText(/输入新 Key 描述名称/), 'New integration');
    await createKey();
    expect(await screen.findByText('New integration')).toBeTruthy();
    expect(getApiKey()).toBe(CREATED_KEY);
    expect(gateway.afterCreate()[0].authorization).toBe(`Bearer ${CREATED_KEY}`);
    const row = screen.getByText('Previous client').closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: /注\s*销/ }));
    await userEvent.click(await screen.findByRole('button', { name: /确\s*定/ }));
    await waitFor(() => expect(screen.queryByText('Previous client')).toBeNull());
    expect(gateway.requests.at(-1)?.authorization).toBe(`Bearer ${CREATED_KEY}`);
    expect(getApiKey()).toBe(CREATED_KEY);
    expect(authFailure).not.toHaveBeenCalled();

    view.unmount();
    vi.resetModules();
    const reloaded = await import('../src/infrastructure/http/client');
    reloaded.httpClient.defaults.adapter = gateway.adapter;
    await reloaded.httpClient.get('/admin/apikeys');
    expect(gateway.requests.at(-1)?.authorization).toBe(`Bearer ${CREATED_KEY}`);
  });

  it('replaces a stale browser credential when bootstrapping a reset database', async () => {
    setApiKey(OTHER_KEY);
    await openModal();
    await createKey();
    expect(await screen.findByText('新访问密钥')).toBeTruthy();
    expect(gateway.requests.find((r) => r.method === 'POST')?.authorization).toBe(`Bearer ${OTHER_KEY}`);
    expect(getApiKey()).toBe(CREATED_KEY);
    expect(gateway.afterCreate()[0].authorization).toBe(`Bearer ${CREATED_KEY}`);
    expect(authFailure).not.toHaveBeenCalled();
  });

  it.each([
    ['configured admin key', 'synthetic-config-admin-for-tests-only'],
    ['administrator session', 'adm_synthetic-session-for-tests-only'],
  ])('keeps the remote permission boundary after creating with a %s', async (_name, credential) => {
    gateway.remote = true;
    gateway.privilegedKeys.add(credential);
    setApiKey(credential);
    await openModal();
    await createKey();
    // Existing behavior selects a generated ordinary key. It grants /v1 access,
    // but must not gain remote /admin privileges as a side effect of this fix.
    expect(await screen.findByText(/Synthetic admin credential required/)).toBeTruthy();
    expect(getApiKey()).toBe(CREATED_KEY);
    expect(gateway.requests.find((r) => r.method === 'POST')?.authorization).toBe(`Bearer ${credential}`);
    expect(gateway.afterCreate()[0].authorization).toBe(`Bearer ${CREATED_KEY}`);
    expect(authFailure).toHaveBeenCalledWith('Synthetic admin credential required');
    await expect(httpClient.get('/v1/models')).resolves.toMatchObject({ status: 200 });
  });

  it.each(['', OTHER_KEY])('leaves credential %j unchanged when creation fails', async (credential) => {
    if (credential) {
      gateway.keys.push(keyItem(credential, 'Existing client'));
      setApiKey(credential);
    }
    gateway.rejectCreate = true;
    await openModal();
    await userEvent.click(screen.getByRole('button', { name: /生成新 Key/ }));
    await waitFor(() => expect(message.error).toHaveBeenCalledWith('生成失败: Synthetic create failed'));
    expect(getApiKey()).toBe(credential);
    expect(gateway.afterCreate()).toHaveLength(0);
    expect(message.success).not.toHaveBeenCalled();
  });

  it('saves a manually entered key before retrying the protected list', async () => {
    gateway.keys.push(keyItem(OTHER_KEY, 'Existing client'));
    await openModal();
    expect(await screen.findByText(/无法读取密钥列表/)).toBeTruthy();
    authFailure.mockClear();
    await userEvent.type(screen.getByPlaceholderText(/已有 API Key/), `  ${OTHER_KEY}  `);
    await userEvent.click(screen.getByRole('button', { name: /使用此 Key/ }));
    expect(await screen.findByText('Existing client')).toBeTruthy();
    expect(getApiKey()).toBe(OTHER_KEY);
    expect(gateway.requests.at(-1)?.authorization).toBe(`Bearer ${OTHER_KEY}`);
    expect(authFailure).not.toHaveBeenCalled();
  });

  it('allows an explicit switch to another listed key', async () => {
    gateway.keys.push(keyItem(OTHER_KEY, 'Existing client'), keyItem(CREATED_KEY, 'Selected client'));
    setApiKey(OTHER_KEY);
    await openModal();
    const row = (await screen.findByText('Selected client')).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: /设为默认/ }));
    await userEvent.click(await screen.findByRole('button', { name: /确\s*定/ }));
    await waitFor(() => expect(getApiKey()).toBe(CREATED_KEY));
    await httpClient.get('/admin/apikeys');
    expect(gateway.requests.at(-1)?.authorization).toBe(`Bearer ${CREATED_KEY}`);
    expect(authFailure).not.toHaveBeenCalled();
  });

  it('deletes a non-current key and refreshes with the unchanged active credential', async () => {
    gateway.keys.push(keyItem(OTHER_KEY, 'Keep this client'), keyItem(CREATED_KEY, 'Remove this client'));
    setApiKey(OTHER_KEY);
    await openModal();
    const row = (await screen.findByText('Remove this client')).closest('tr')!;
    await userEvent.click(within(row).getByRole('button', { name: /注\s*销/ }));
    await userEvent.click(await screen.findByRole('button', { name: /确\s*定/ }));
    await waitFor(() => expect(screen.queryByText('Remove this client')).toBeNull());
    const deletionIndex = gateway.requests.findIndex((r) => r.method === 'DELETE');
    expect(deletionIndex).toBeGreaterThan(-1);
    expect(gateway.requests.slice(deletionIndex).map((r) => r.authorization))
      .toEqual([`Bearer ${OTHER_KEY}`, `Bearer ${OTHER_KEY}`]);
    expect(getApiKey()).toBe(OTHER_KEY);
    expect(authFailure).not.toHaveBeenCalled();
  });
});
