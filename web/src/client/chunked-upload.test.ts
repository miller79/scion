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

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import {
  _setUploadRetryDelayForTest,
  DEFAULT_UPLOAD_LIMITS,
  fitsOneRequest,
  formatUploadSize,
  getUploadLimits,
  uploadFileChunked,
  uploadWorkspaceFiles,
  UploadHttpError,
} from './chunked-upload.js';
import { invalidatePublicSettings } from './server-feature-flags.js';
import { requestUrl } from './__fixtures__/request-url.js';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface Recorded {
  op: string | null;
  offset: number | null;
  size: number;
  url: string;
}

/**
 * A hub that runs chunked uploads in memory: it checks offsets as the hub
 * does and keeps the bytes, so a test can compare them with the input.
 * `failChunk` makes the given chunk call (by call number) fail once.
 */
function fakeHub(opts: {
  chunkSize: number;
  uploads?: Record<string, number>;
  failChunkCall?: { call: number; kind: 'network' | 'status' | 'busy'; status?: number };
  /** Answer every chunk with received = its offset (nothing arrived). */
  noProgress?: boolean;
  /** Fail the first N finish calls (a gateway timeout). */
  failFinishCalls?: number;
  /** Answer the first N finish calls busy (the hub still storing the file). */
  busyFinishCalls?: number;
  /** Refuse every chunk with this status and message. */
  refuseChunks?: { status: number; message: string };
}) {
  const calls: Recorded[] = [];
  let received = new Uint8Array(0);
  let chunkCalls = 0;
  let finishCalls = 0;
  const fetchMock = vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const href = requestUrl(input);
    const url = new URL(href, 'http://hub.test');
    if (url.pathname === '/api/v1/settings/public') {
      return json({ uploads: opts.uploads ?? {} });
    }
    const op = url.searchParams.get('upload');
    const offset = url.searchParams.has('offset') ? Number(url.searchParams.get('offset')) : null;
    const body = init?.body;
    const size = body instanceof Blob ? body.size : 0;
    calls.push({ op, offset, size, url: href });
    switch (op) {
      case 'start':
        return json(
          { uploadId: 'u1', chunkSize: opts.chunkSize, maxSize: 1 << 30, received: 0 },
          201
        );
      case 'chunk': {
        chunkCalls++;
        if (opts.refuseChunks) {
          return json({ error: { message: opts.refuseChunks.message } }, opts.refuseChunks.status);
        }
        if (opts.failChunkCall && opts.failChunkCall.call === chunkCalls) {
          if (opts.failChunkCall.kind === 'network') throw new TypeError('Failed to fetch');
          if (opts.failChunkCall.kind === 'busy') {
            return json({ error: { code: 'upload_busy', message: 'busy' } }, 409);
          }
          return json({ error: { message: 'busy' } }, opts.failChunkCall.status ?? 502);
        }
        if (offset !== received.length) {
          return json(
            {
              error: {
                code: 'upload_offset_mismatch',
                message: 'mismatch',
                details: { received: received.length },
              },
            },
            409
          );
        }
        if (opts.noProgress) return json({ received: offset });
        const bytes = new Uint8Array(await (body as Blob).arrayBuffer());
        const next = new Uint8Array(received.length + bytes.length);
        next.set(received);
        next.set(bytes, received.length);
        received = next;
        return json({ received: received.length });
      }
      case 'finish':
        if (finishCalls++ < (opts.failFinishCalls ?? 0)) {
          return json({ error: { message: 'gateway timeout' } }, 504);
        }
        if (finishCalls <= (opts.busyFinishCalls ?? 0)) {
          return json({ error: { code: 'upload_busy', message: 'busy' } }, 409);
        }
        return json({ files: [{ path: 'x', size: received.length }] });
      case 'abort':
        return new Response(null, { status: 204 });
      default:
        // A one-request (multipart) upload.
        return json({ files: [] });
    }
  });
  vi.stubGlobal('fetch', fetchMock);
  return { calls, received: () => received };
}

function fileOf(size: number, name = 'f.bin'): File {
  const data = new Uint8Array(size);
  for (let i = 0; i < size; i++) data[i] = (i * 31 + 7) % 251;
  return new File([data], name);
}

describe('chunked-upload', () => {
  beforeEach(() => {
    invalidatePublicSettings();
    _setUploadRetryDelayForTest(0);
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    invalidatePublicSettings();
  });

  it('reads the limits from the hub, with defaults for missing fields', async () => {
    fakeHub({ chunkSize: 8, uploads: { maxWorkspaceFileSize: 1 << 30, chunkSize: 4 << 20 } });
    expect(await getUploadLimits()).toEqual({
      chunkSize: 4 << 20,
      maxAttachmentSize: DEFAULT_UPLOAD_LIMITS.maxAttachmentSize,
      maxWorkspaceFileSize: 1 << 30,
    });
  });

  it('formats sizes for messages', () => {
    expect(formatUploadSize(1 << 30)).toBe('1 GiB');
    expect(formatUploadSize(50 * 1024 * 1024)).toBe('50 MiB');
    expect(formatUploadSize(1536)).toBe('1.5 KiB');
    expect(formatUploadSize(900)).toBe('900 B');
  });

  it('decides one request versus chunks from the chunk size', () => {
    expect(fitsOneRequest([fileOf(4), fileOf(4)], 2048 + 8)).toBe(true);
    expect(fitsOneRequest([fileOf(4), fileOf(5)], 2048 + 8)).toBe(false);
  });

  it('splits a file into chunks, reports progress and sends the exact bytes', async () => {
    const hub = fakeHub({ chunkSize: 10 });
    const file = fileOf(35);
    const progress: Array<[number, number]> = [];
    const res = await uploadFileChunked('/api/v1/projects/p/workspace/files', file, {
      name: 'dir/f.bin',
      onProgress: (sent, total) => progress.push([sent, total]),
    });
    expect(res.ok).toBe(true);
    expect(hub.calls.map((c) => [c.op, c.offset, c.size])).toEqual([
      ['start', null, 0],
      ['chunk', 0, 10],
      ['chunk', 10, 10],
      ['chunk', 20, 10],
      ['chunk', 30, 5],
      ['finish', null, 0],
    ]);
    expect(progress).toEqual([
      [0, 35],
      [10, 35],
      [20, 35],
      [30, 35],
      [35, 35],
    ]);
    expect(Array.from(hub.received())).toEqual(
      Array.from(new Uint8Array(await file.arrayBuffer()))
    );
    expect(hub.calls.every((c) => c.size <= 10)).toBe(true);
  });

  it('sends query parameters on every call', async () => {
    const hub = fakeHub({ chunkSize: 10 });
    await uploadFileChunked('/api/v1/chat/attachments', fileOf(15), {
      query: { project_id: 'proj-1' },
    });
    expect(hub.calls.every((c) => c.url.includes('project_id=proj-1'))).toBe(true);
  });

  it('retries a chunk once after a network error', async () => {
    const hub = fakeHub({ chunkSize: 10, failChunkCall: { call: 2, kind: 'network' } });
    const file = fileOf(25);
    const res = await uploadFileChunked('/u', file);
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.op === 'chunk').map((c) => c.offset)).toEqual([0, 10, 10, 20]);
    expect(Array.from(hub.received())).toEqual(
      Array.from(new Uint8Array(await file.arrayBuffer()))
    );
  });

  it('retries a chunk once after a 5xx', async () => {
    const hub = fakeHub({ chunkSize: 10, failChunkCall: { call: 1, kind: 'status', status: 502 } });
    const res = await uploadFileChunked('/u', fileOf(12));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.op === 'chunk').map((c) => c.offset)).toEqual([0, 0, 10]);
  });

  it('resumes from the offset the hub reports after a lost response', async () => {
    const hub = fakeHub({ chunkSize: 10 });
    // Simulate a lost response: the hub already holds the first chunk.
    await uploadFileChunked('/u', fileOf(10));
    hub.calls.length = 0;
    const res = await uploadFileChunked('/u', fileOf(30));
    // The fake hub keeps its 10 bytes: the first chunk is refused with 409
    // received=10, and the upload continues from there.
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.op === 'chunk').map((c) => c.offset)).toEqual([0, 10, 20]);
  });

  it('stops on a refusal it cannot retry and cancels the upload', async () => {
    const hub = fakeHub({
      chunkSize: 10,
      refuseChunks: { status: 413, message: 'Upload exceeds its declared size' },
    });
    await expect(uploadFileChunked('/u', fileOf(15))).rejects.toThrow(
      'Upload exceeds its declared size'
    );
    expect(hub.calls.map((c) => c.op)).toContain('abort');
  });

  it('refuses a workspace file over the hub limit before sending, naming the limit', async () => {
    const hub = fakeHub({ chunkSize: 10, uploads: { maxWorkspaceFileSize: 20, chunkSize: 10 } });
    const err = await uploadWorkspaceFiles('/w', [{ path: 'big.bin', file: fileOf(21) }]).catch(
      (e: unknown) => e
    );
    expect(err).toBeInstanceOf(UploadHttpError);
    expect((err as UploadHttpError).status).toBe(413);
    expect((err as Error).message).toBe('File "big.bin" exceeds the maximum size of 20 B');
    expect(hub.calls).toEqual([]);
  });

  it('sends small workspace files in one request and large ones in chunks', async () => {
    // One request holds 2058 bytes: two small files and their framing fit.
    const hub = fakeHub({
      chunkSize: 100,
      uploads: { maxWorkspaceFileSize: 4096, chunkSize: 2058 },
    });
    await uploadWorkspaceFiles('/w', [
      { path: 'a', file: fileOf(4) },
      { path: 'b', file: fileOf(5) },
    ]);
    expect(hub.calls.map((c) => c.op)).toEqual([null]);

    hub.calls.length = 0;
    const progress: number[] = [];
    await uploadWorkspaceFiles('/w', [{ path: 'c', file: fileOf(1100) }], (sent) =>
      progress.push(sent)
    );
    const ops = hub.calls.map((c) => c.op);
    expect(ops[0]).toBe('start');
    expect(ops.at(-1)).toBe('finish');
    expect(ops.filter((o) => o === 'chunk').length).toBe(11);
    expect(progress.at(-1)).toBe(1100);
  });

  it('waits out a busy answer instead of failing the upload', async () => {
    const hub = fakeHub({
      chunkSize: 10,
      failChunkCall: { call: 1, kind: 'busy' },
    });
    const res = await uploadFileChunked('/u', fileOf(12));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.op === 'chunk').map((c) => c.offset)).toEqual([0, 0, 10]);
  });

  it('fails instead of looping when a chunk makes no progress', async () => {
    fakeHub({ chunkSize: 10, noProgress: true });
    await expect(uploadFileChunked('/u', fileOf(12))).rejects.toThrow('received no data');
  });

  it('keeps repeating a busy finish while the hub stores the file', async () => {
    const hub = fakeHub({ chunkSize: 10, busyFinishCalls: 9 });
    const res = await uploadFileChunked('/u', fileOf(5));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.op === 'finish').length).toBe(10);
  });

  it('repeats a finish whose response was lost', async () => {
    const hub = fakeHub({ chunkSize: 10, failFinishCalls: 2 });
    const res = await uploadFileChunked('/u', fileOf(5));
    expect(res.ok).toBe(true);
    expect(hub.calls.filter((c) => c.op === 'finish').length).toBe(3);
  });
});
