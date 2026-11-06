/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The /agents table's Messaging cell keeps the message-mode badge and the
 * messageability arrow together in one non-wrapping row, so the wider
 * "Project" badge no longer pushes the arrow onto a second line.
 */

import { describe, it, expect, beforeAll, beforeEach, afterEach, vi } from 'vitest';
vi.mock('../../utils/toast.js', () => ({ showToast: vi.fn() }));

import './agents.js';
import type { ScionPageAgents } from './agents.js';
import { stateManager } from '../../client/state.js';
import type { Agent } from '../../shared/types.js';

/** happy-dom has no EventSource; setScope opens one. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  readyState = FakeEventSource.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(readonly url: string) {
    super();
  }
  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }
}

beforeAll(() => {
  const store = new Map<string, string>();
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
  });
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function agent(id: string, overrides: Partial<Agent> = {}): Agent {
  return {
    id,
    name: id,
    projectId: 'p1',
    template: 't',
    phase: 'running',
    created: '2026-01-01T00:00:00Z',
    updated: '2026-01-01T00:00:00Z',
    messageMode: 'project',
    _capabilities: { actions: ['read'] },
    ...overrides,
  } as Agent;
}

async function flush(): Promise<void> {
  // state.ts flushes after 100ms with no animation frame.
  await new Promise((r) => setTimeout(r, 150));
}

type TestEl = ScionPageAgents & { agents: Agent[] };

async function mount(initial: Agent[]): Promise<TestEl> {
  vi.stubGlobal(
    'fetch',
    vi.fn(() => Promise.resolve(jsonResponse({ agents: initial, _capabilities: { actions: [] } })))
  );
  const el = document.createElement('scion-page-agents') as TestEl;
  document.body.appendChild(el);
  await el.updateComplete;
  await flush();
  await el.updateComplete;
  return el;
}

/** The table row of the agent named `name`. */
function rowFor(el: TestEl, name: string): HTMLTableRowElement | null {
  const link = Array.from(el.shadowRoot!.querySelectorAll<HTMLAnchorElement>('a')).find(
    (a) => a.getAttribute('href') === `/agents/${name}`
  );
  return (link?.closest('tr') as HTMLTableRowElement | null) ?? null;
}

describe('scion-page-agents Messaging cell', () => {
  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    stateManager.setScope({ type: 'brokers-list' });
    localStorage.setItem('scion-view-agents', 'list');
  });

  afterEach(() => {
    document.body.querySelectorAll('scion-page-agents').forEach((n) => n.remove());
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  for (const mode of ['project', 'hub'] as const) {
    it(`${mode}: puts the badge and the arrow in one non-wrapping cell`, async () => {
      const el = await mount([
        agent('a1', {
          messageMode: mode,
          _messageability: { canMessage: true, canReachViewer: true },
        } as Partial<Agent>),
      ]);

      const cell = rowFor(el, 'a1')?.querySelector('.messaging-cell');
      expect(cell).not.toBeNull();
      expect(cell?.querySelector('scion-message-mode-badge')?.getAttribute('mode')).toBe(mode);
      expect(cell?.querySelector('scion-messageability-indicator')).not.toBeNull();

      const css = (
        el.constructor as unknown as { styles: { cssText?: string }[] | { cssText?: string } }
      ).styles;
      const text = (Array.isArray(css) ? css : [css]).map((s) => s.cssText ?? '').join('\n');
      const rule = /\.messaging-cell\s*\{([^}]*)\}/.exec(text)?.[1] ?? '';
      expect(rule).toMatch(/white-space:\s*nowrap/);
      expect(rule).toMatch(/align-items:\s*center/);
    });
  }

  it('renders the badge alone when the row has no messageability', async () => {
    const el = await mount([agent('a1')]);
    const cell = rowFor(el, 'a1')?.querySelector('.messaging-cell');
    expect(cell?.querySelector('scion-message-mode-badge')).not.toBeNull();
    expect(cell?.querySelector('scion-messageability-indicator')).toBeNull();
  });
});
