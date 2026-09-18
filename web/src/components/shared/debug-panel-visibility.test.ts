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
 * Visibility rules for the floating debug control (#104).
 *
 * The toggle is fixed to the viewport corner above everything else, and in
 * chat both bottom corners are scrollable interactive lists (members sidebar,
 * space rail) — so while it is displayed it covers controls underneath, and
 * there is no corner it can move to that fixes that. It therefore has to be
 * displayed only when someone actually wants it.
 *
 * Two independent conditions must hold: the viewer opted in, and the Hub
 * actually serves `/auth/debug`. Both fail closed.
 */

// @vitest-environment happy-dom

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import './debug-panel.js';

const PREF_KEY = 'scion.debugPanel';

/** Mount a fresh panel and let its connected-callback work settle. */
async function mountPanel(): Promise<HTMLElement> {
  const el = document.createElement('scion-debug-panel');
  document.body.appendChild(el);
  // One turn for the availability probe, one for the resulting render.
  await new Promise((r) => setTimeout(r, 0));
  await (el as HTMLElement & { updateComplete: Promise<unknown> }).updateComplete;
  return el;
}

/** Everything the component renders lives in its shadow root. */
function renderedText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function setSearch(search: string): void {
  window.history.replaceState({}, '', `/${search}`);
}

/** Hub serves the debug endpoint. */
function serveDebug(ok: boolean): void {
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue({ ok, status: ok ? 200 : 404, json: async () => ({}) })
  );
}

beforeEach(() => {
  localStorage.clear();
  setSearch('');
  serveDebug(true);
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
  localStorage.clear();
});

describe('debug control visibility', () => {
  it('renders nothing by default, so it cannot cover the UI beneath it', async () => {
    const el = await mountPanel();
    expect(renderedText(el)).not.toContain('Debug');
  });

  it('does not even ask the Hub when the viewer has not opted in', async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);

    await mountPanel();

    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('appears when the viewer opts in with ?debug=1 and the Hub serves it', async () => {
    setSearch('?debug=1');
    const el = await mountPanel();
    expect(renderedText(el)).toContain('Debug');
  });

  it('stays hidden when the viewer opted in but the Hub has no debug endpoint', async () => {
    // A Hub started without --debug answers 404. Previously the control was
    // rendered anyway and only vanished after someone clicked it.
    setSearch('?debug=1');
    serveDebug(false);

    const el = await mountPanel();

    expect(renderedText(el)).not.toContain('Debug');
  });

  it('stays hidden when the availability probe fails outright', async () => {
    setSearch('?debug=1');
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('network')));

    const el = await mountPanel();

    expect(renderedText(el)).not.toContain('Debug');
  });
});

describe('the opt-in persists per browser', () => {
  it('remembers the choice without the query parameter', async () => {
    setSearch('?debug=1');
    await mountPanel();
    document.body.innerHTML = '';

    setSearch('');
    const el = await mountPanel();

    expect(renderedText(el)).toContain('Debug');
  });

  it('?debug=0 turns it back off and clears the stored preference', async () => {
    localStorage.setItem(PREF_KEY, '1');
    setSearch('?debug=0');

    const el = await mountPanel();

    expect(renderedText(el)).not.toContain('Debug');
    expect(localStorage.getItem(PREF_KEY)).toBeNull();
  });

  it('survives storage being unavailable, failing closed', async () => {
    // Private windows and blocked site data make these throw.
    const boom = () => {
      throw new Error('denied');
    };
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(boom);
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(boom);

    const el = await mountPanel();

    expect(renderedText(el)).not.toContain('Debug');
    vi.restoreAllMocks();
  });
});
