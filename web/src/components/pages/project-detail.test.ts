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
 * Tests for project-detail.ts: Clone / Create Template make a new project,
 * so they need project read AND hub-scope project.create (design §5.F).
 */

import { describe, it, expect, vi, beforeAll, beforeEach, afterEach } from 'vitest';

import type { Capabilities, PageData, UserRole } from '../../shared/types.js';
import { resetHubProjectCapabilitiesCache } from '../../client/hub-capabilities.js';

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

const PROJECT_ID = 'p-1';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface ComponentOpts {
  projectCaps: Capabilities;
  hubCaps?: Capabilities;
  sessionSummary?: Record<string, unknown>;
}

function createFetchHandler(opts: ComponentOpts) {
  return (url: string | URL | Request): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url;
    if (opts.sessionSummary && path.endsWith(`/api/v1/projects/${PROJECT_ID}/metrics/summary`)) {
      return Promise.resolve(jsonResponse(opts.sessionSummary));
    }
    if (path.includes('/api/v1/projects?limit=1')) {
      return Promise.resolve(
        jsonResponse({ projects: [], ...(opts.hubCaps ? { _capabilities: opts.hubCaps } : {}) })
      );
    }
    if (path.includes(`/api/v1/projects/${PROJECT_ID}/agents`)) {
      return Promise.resolve(jsonResponse({ agents: [], _capabilities: { actions: [] } }));
    }
    if (path.endsWith(`/api/v1/projects/${PROJECT_ID}`)) {
      return Promise.resolve(
        jsonResponse({
          id: PROJECT_ID,
          name: 'Project One',
          slug: 'project-one',
          _capabilities: opts.projectCaps,
        })
      );
    }
    return Promise.resolve(jsonResponse({}, 404));
  };
}

async function createComponent(
  opts: ComponentOpts,
  role: UserRole = 'member'
): Promise<HTMLElement> {
  vi.stubGlobal('fetch', vi.fn(createFetchHandler(opts)));
  const el = document.createElement('scion-page-project-detail') as HTMLElement & {
    updateComplete: Promise<boolean>;
    pageData: PageData | null;
    projectId: string;
  };
  el.projectId = PROJECT_ID;
  el.pageData = {
    path: `/projects/${PROJECT_ID}`,
    title: 'Project',
    user: { id: 'u', email: 'u@example.com', name: 'U', role },
  };
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

function headerActionsText(el: HTMLElement): string {
  return el.shadowRoot?.querySelector('.header-actions')?.textContent?.replace(/\s+/g, ' ') ?? '';
}

function hasClone(el: HTMLElement): boolean {
  return /\bClone\b/.test(headerActionsText(el));
}

describe('scion-page-project-detail — Clone / Create Template gating', () => {
  let element: HTMLElement | null = null;

  // project-detail pulls in many sub-components; import it once, outside the
  // per-test timeout, so a loaded CI machine does not time out the first test.
  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders the page header (sanity)', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      hubCaps: { actions: ['create'] },
    });
    expect(element.shadowRoot?.querySelector('.header-actions')).not.toBeNull();
    expect(headerActionsText(element)).toContain('Metrics');
  });

  it('shows Clone with project read and hub create (member)', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      hubCaps: { actions: ['list', 'create'] },
    });
    expect(hasClone(element)).toBe(true);
  });

  it('hides Clone without hub create (viewer)', async () => {
    element = await createComponent(
      { projectCaps: { actions: ['read'] }, hubCaps: { actions: ['list'] } },
      'viewer'
    );
    expect(hasClone(element)).toBe(false);
    // Unrelated header actions still render.
    expect(headerActionsText(element)).toContain('Metrics');
  });

  it('hides Clone without project read even with hub create', async () => {
    element = await createComponent({
      projectCaps: { actions: [] },
      hubCaps: { actions: ['create'] },
    });
    expect(hasClone(element)).toBe(false);
  });

  it('hides Clone when hub caps cannot be loaded (fail-closed)', async () => {
    element = await createComponent({ projectCaps: { actions: ['read'] } });
    expect(hasClone(element)).toBe(false);
  });

  it('admin with hub create gets the Clone / Create Template dropdown', async () => {
    element = await createComponent(
      { projectCaps: { actions: ['read'] }, hubCaps: { actions: ['create'] } },
      'admin'
    );
    const text = headerActionsText(element);
    expect(text).toContain('Clone Project');
    expect(text).toContain('Create Template');
  });

  it('hides Create Template without hub create, regardless of admin role string', async () => {
    element = await createComponent(
      { projectCaps: { actions: ['read'] }, hubCaps: { actions: [] } },
      'admin'
    );
    const text = headerActionsText(element);
    expect(text).not.toContain('Create Template');
    expect(hasClone(element)).toBe(false);
  });
});

describe('scion-page-project-detail — session summary stats', () => {
  let element: HTMLElement | null = null;

  beforeAll(async () => {
    await import('./project-detail.js');
  }, 60_000);

  beforeEach(() => {
    vi.stubGlobal('EventSource', FakeEventSource);
    resetHubProjectCapabilitiesCache();
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  const summary = {
    projectId: PROJECT_ID,
    totalSessions: 12,
    totalTokensInput: 4000,
    totalTokensOutput: 1000,
    totalTokensCached: 0,
    totalTokensReasoning: 0,
    activeAgents: 4,
    mostUsedTools: [],
    mostUsedModels: [],
  };

  function statLabels(el: HTMLElement): string[] {
    return Array.from(el.shadowRoot?.querySelectorAll('.stat-label') ?? []).map(
      (n) => n.textContent?.trim() ?? ''
    );
  }

  it('names the window when the figures come from telemetry', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      sessionSummary: { ...summary, source: 'telemetry', periodDays: 30 },
    });
    const labels = statLabels(element);
    expect(labels).toEqual(
      expect.arrayContaining(['Sessions (30d)', 'Tokens (30d)', 'Agents (30d)'])
    );
    expect(labels).not.toContain('Total Sessions');
  });

  it('shows the telemetry token total, which has no input/output split', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      sessionSummary: {
        ...summary,
        totalTokensInput: 0,
        totalTokensOutput: 0,
        totalTokens: 2_500_000,
        source: 'telemetry',
        periodDays: 30,
      },
    });
    const values = Array.from(element.shadowRoot?.querySelectorAll('.stat') ?? [])
      .filter((s) => s.querySelector('.stat-label')?.textContent?.trim() === 'Tokens (30d)')
      .map((s) => s.querySelector('.stat-value')?.textContent?.trim());
    expect(values).toEqual(['2.5M']);
  });

  it('keeps the all-time labels for stored session reports', async () => {
    element = await createComponent({
      projectCaps: { actions: ['read'] },
      sessionSummary: { ...summary, source: 'sessions' },
    });
    expect(statLabels(element)).toEqual(
      expect.arrayContaining(['Total Sessions', 'Total Tokens', 'Active Agents'])
    );
  });
});
