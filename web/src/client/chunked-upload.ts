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
 * Upload limits and chunked uploads (pkg/hub/chunked_upload.go).
 *
 * The hub publishes its upload limits in `GET /api/v1/settings/public`
 * (`uploads`). Files the hub's one-request limits take go in one multipart
 * request, as before chunked uploads existed. Anything larger is sent in
 * requests of at most one chunk each, through the same upload route
 * (`?upload=start|chunk|finish|abort`), so a reverse proxy in front of the
 * hub only has to accept one chunk per request. A chunked upload lives in
 * one hub instance's memory, so it is used only when one request cannot
 * take the files: on a hub with several instances every call of it must
 * reach the same one (session affinity).
 */

import { apiFetch, parseApiError } from './api.js';
import { getPublicSettings } from './server-feature-flags.js';

/** Upload limits in bytes (`PublicUploadLimits` in the hub). */
export interface UploadLimits {
  chunkSize: number;
  maxAttachmentSize: number;
  maxWorkspaceFileSize: number;
}

/** The hub defaults, used when the hub does not publish its limits. */
export const DEFAULT_UPLOAD_LIMITS: Readonly<UploadLimits> = Object.freeze({
  chunkSize: 8 * 1024 * 1024,
  maxAttachmentSize: 10 * 1024 * 1024,
  maxWorkspaceFileSize: 50 * 1024 * 1024,
});

function positive(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : fallback;
}

/** The hub's upload limits, or the defaults for a field it does not send. */
export async function getUploadLimits(): Promise<UploadLimits> {
  const settings = await getPublicSettings();
  const u = settings?.uploads;
  return {
    chunkSize: positive(u?.chunkSize, DEFAULT_UPLOAD_LIMITS.chunkSize),
    maxAttachmentSize: positive(u?.maxAttachmentSize, DEFAULT_UPLOAD_LIMITS.maxAttachmentSize),
    maxWorkspaceFileSize: positive(
      u?.maxWorkspaceFileSize,
      DEFAULT_UPLOAD_LIMITS.maxWorkspaceFileSize
    ),
  };
}

/** A byte count for a message: "10 MiB", "1 GiB", "1.5 GiB", "900 B". */
export function formatUploadSize(bytes: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let value = bytes;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  const text = Number.isInteger(value) ? String(value) : value.toFixed(1);
  return `${text} ${units[i]}`;
}

/** The message for a file over the per-file limit. */
export function fileTooLargeMessage(name: string, limit: number): string {
  return `File "${name}" exceeds the maximum size of ${formatUploadSize(limit)}`;
}

/** Room left in a one-request upload for the multipart framing of each file. */
const MULTIPART_OVERHEAD_PER_FILE = 1024;

/**
 * Whether the files can go in one request: all of them together, with
 * their multipart framing, within bodyLimit bytes.
 */
export function fitsOneRequest(files: ArrayLike<Blob>, bodyLimit: number): boolean {
  let total = 0;
  for (let i = 0; i < files.length; i++) total += files[i].size + MULTIPART_OVERHEAD_PER_FILE;
  return total <= bodyLimit;
}

/**
 * The hub's one-request upload limits, which it still applies to a
 * multipart upload:
 * - workspaceBody: a workspace or shared directory upload body is capped at
 *   maxUploadTotalSize, 100 MiB (pkg/hub/project_workspace_handlers.go);
 * - chat: an attachment upload takes at most MaxAttachmentsPerMessage
 *   (chatFiles) files, in a body of chatFiles × min(per-file limit,
 *   MaxAttachmentSize = chatFileCap) plus chatFormRoom for the form fields
 *   (pkg/hub/handlers_chat_v2.go). The cap is larger when attachments go to
 *   the artifact service, which the client cannot tell, so the smaller one
 *   is used.
 */
const ONE_REQUEST_LIMITS = Object.freeze({
  workspaceBody: 100 * 1024 * 1024,
  chatFiles: 10,
  chatFileCap: 10 * 1024 * 1024,
  chatFormRoom: 1024 * 1024,
});

let oneRequestLimits: typeof ONE_REQUEST_LIMITS = ONE_REQUEST_LIMITS;

/** For tests: lower the hub's one-request limits; null restores them. */
export function _setOneRequestLimitsForTest(
  limits: Partial<typeof ONE_REQUEST_LIMITS> | null
): void {
  oneRequestLimits = { ...ONE_REQUEST_LIMITS, ...(limits ?? {}) };
}

const utf8 = new TextEncoder();

/**
 * Whether a workspace or shared directory upload of entries fits one
 * request. Each part names its path twice (field name and file name), so
 * the path's bytes are counted on top of the framing, three times over for
 * characters the browser escapes.
 */
export function fitsOneWorkspaceRequest(
  entries: ReadonlyArray<{ path: string; file: Blob }>
): boolean {
  let total = 0;
  for (const e of entries) {
    total += e.file.size + MULTIPART_OVERHEAD_PER_FILE + 6 * utf8.encode(e.path).length;
  }
  return total <= oneRequestLimits.workspaceBody;
}

/**
 * Whether a chat attachment upload of files fits one request, for a hub
 * whose per-file attachment limit is maxAttachmentSize.
 */
export function fitsOneChatRequest(files: ArrayLike<Blob>, maxAttachmentSize: number): boolean {
  const { chatFiles, chatFileCap, chatFormRoom } = oneRequestLimits;
  return (
    files.length <= chatFiles &&
    fitsOneRequest(files, Math.min(maxAttachmentSize, chatFileCap) * chatFiles + chatFormRoom)
  );
}

/** An upload the hub refused; `status` is the HTTP status, `code` the hub's error code. */
export class UploadHttpError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly code = ''
  ) {
    super(message);
    this.name = 'UploadHttpError';
  }
}

/** Wait before a retry, doubled for each attempt; a variable so tests can shorten it. */
let retryDelayMs = 1000;

/** For tests: set the base retry delay. */
export function _setUploadRetryDelayForTest(ms: number): void {
  retryDelayMs = ms;
}

function retryWait(attempt: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, retryDelayMs * 2 ** attempt));
}

/** How often a chunk the hub reports busy (an earlier try still running) is repeated. */
const BUSY_RETRIES = 6;

/** The longest wait between two finish calls. */
const FINISH_MAX_WAIT_MS = 10_000;

/**
 * How long finish is repeated while the hub reports it busy: as long as the
 * hub gives itself to store the file (chunkedUploadFinishWriteBudget).
 */
let finishWindowMs = 15 * 60 * 1000;

/** For tests: set how long finish is repeated while busy. */
export function _setFinishWindowForTest(ms: number): void {
  finishWindowMs = ms;
}

async function refusal(res: Response): Promise<UploadHttpError> {
  const info = await parseApiError(res, `Upload failed: HTTP ${res.status}`);
  return new UploadHttpError(info.message, res.status, info.code);
}

export interface ChunkedUploadOptions {
  /** The file name (a relative path for workspace uploads); default the file's name. */
  name?: string;
  /** Query parameters sent on every call, e.g. `project_id`. */
  query?: Record<string, string>;
  /** Called with the bytes the hub has received, and the file size. */
  onProgress?: (sent: number, total: number) => void;
}

/**
 * Upload `file` to the upload route `url` in chunks. Each chunk is retried
 * once after a network error or a 5xx; when the hub reports another offset
 * (a lost response), the upload continues from there, and while it reports
 * an earlier try of the same call still running, the call is repeated after
 * a pause. The finish call is repeated the same way: the hub answers a
 * repeated finish with the first one's response. Resolves to the finish
 * response, which is the response a one-request upload of the file would
 * get; rejects with an UploadHttpError (or a network Error) when the upload
 * could not be sent, after cancelling it on the hub.
 */
export async function uploadFileChunked(
  url: string,
  file: Blob & { name?: string },
  options: ChunkedUploadOptions = {}
): Promise<Response> {
  const name = options.name ?? file.name ?? 'file';
  const callUrl = (op: string, extra: Record<string, string> = {}): string =>
    `${url}?${new URLSearchParams({ ...options.query, upload: op, ...extra }).toString()}`;

  const startRes = await apiFetch(callUrl('start'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ name, size: file.size }),
  });
  if (!startRes.ok) throw await refusal(startRes);
  const start = (await startRes.json()) as { uploadId?: string; chunkSize?: number };
  const id = start.uploadId ?? '';
  const chunkSize = positive(start.chunkSize, DEFAULT_UPLOAD_LIMITS.chunkSize);

  let offset = 0;
  options.onProgress?.(0, file.size);
  try {
    while (offset < file.size) {
      const end = Math.min(offset + chunkSize, file.size);
      offset = await sendChunk(callUrl, id, file, offset, end);
      options.onProgress?.(offset, file.size);
    }
  } catch (err) {
    // Best effort: the hub also removes an idle upload on its own.
    void apiFetch(callUrl('abort', { id }), { method: 'POST' }).catch(() => undefined);
    throw err;
  }
  return finishUpload(callUrl, id);
}

/**
 * Call finish, repeating it after a network error or a 5xx (a proxy that
 * gave up while the hub stores a large file) at most twice, and for as long
 * as the hub stores the file while it answers busy (the earlier call is
 * still storing it). A repeat after the store gets the first call's answer.
 */
async function finishUpload(
  callUrl: (op: string, extra?: Record<string, string>) => string,
  id: string
): Promise<Response> {
  const deadline = Date.now() + finishWindowMs;
  let failures = 0;
  for (let attempt = 0; ; attempt++) {
    const wait = Math.min(retryDelayMs * 2 ** Math.min(attempt, 10), FINISH_MAX_WAIT_MS);
    let res: Response;
    try {
      res = await apiFetch(callUrl('finish', { id }), { method: 'POST' });
    } catch (err) {
      if (++failures > 2) throw err;
      await new Promise((resolve) => setTimeout(resolve, wait));
      continue;
    }
    if (res.ok) return res;
    if (res.status >= 500) {
      if (++failures > 2) return res;
    } else if (res.status === 409) {
      const info = await parseApiError(res.clone(), '');
      if (info.code !== 'upload_busy' || Date.now() + wait > deadline) return res;
    } else {
      return res;
    }
    await new Promise((resolve) => setTimeout(resolve, wait));
  }
}

/**
 * Send bytes [offset, end) of file and return the bytes the hub now holds.
 * A network error or a 5xx is retried once; a busy answer (an earlier try
 * still running) is repeated after a pause; a 409 that names the bytes
 * received returns that offset, so the caller continues from it.
 */
async function sendChunk(
  callUrl: (op: string, extra?: Record<string, string>) => string,
  id: string,
  file: Blob,
  offset: number,
  end: number
): Promise<number> {
  let failures = 0;
  for (let busy = 0; ; ) {
    let res: Response;
    try {
      res = await apiFetch(callUrl('chunk', { id, offset: String(offset) }), {
        method: 'POST',
        headers: { 'Content-Type': 'application/octet-stream' },
        body: file.slice(offset, end),
      });
    } catch {
      if (++failures > 1) throw new Error('Upload failed: network error');
      await retryWait(0);
      continue;
    }
    if (res.ok) {
      const body = (await res.json().catch(() => ({}))) as { received?: unknown };
      const received = typeof body.received === 'number' ? body.received : end;
      if (received <= offset) {
        // Nothing arrived: sending the same bytes again would not help.
        throw new UploadHttpError('Upload failed: the hub received no data', res.status);
      }
      return received;
    }
    const err = await refusal(res.clone());
    const info = await parseApiError(res, '');
    const received = info.details?.received;
    if (
      res.status === 409 &&
      typeof received === 'number' &&
      received >= 0 &&
      received <= file.size &&
      received !== offset
    ) {
      return received;
    }
    if (err.code === 'upload_busy' && busy < BUSY_RETRIES) {
      await retryWait(busy++);
      continue;
    }
    if (res.status < 500 || ++failures > 1) throw err;
    await retryWait(0);
  }
}

/** One file of a workspace upload and its path in the workspace. */
export interface WorkspaceUploadEntry {
  path: string;
  file: File;
}

/**
 * Upload files to a project workspace or shared directory upload route
 * (`.../workspace/files`, `.../shared-dirs/{dir}/files`). Files the hub's
 * one-request limit takes together go in one multipart request, as before;
 * otherwise each file is uploaded in chunks, one after the other, so a
 * failure part way leaves the files before it stored. A file over the hub's
 * per-file limit is refused before anything is sent.
 */
export async function uploadWorkspaceFiles(
  url: string,
  entries: WorkspaceUploadEntry[],
  onProgress?: (sent: number, total: number) => void
): Promise<void> {
  const limits = await getUploadLimits();
  const tooLarge = entries.find((e) => e.file.size > limits.maxWorkspaceFileSize);
  if (tooLarge) {
    throw new UploadHttpError(fileTooLargeMessage(tooLarge.path, limits.maxWorkspaceFileSize), 413);
  }
  const total = entries.reduce((sum, e) => sum + e.file.size, 0);

  if (fitsOneWorkspaceRequest(entries)) {
    const formData = new FormData();
    for (const e of entries) formData.append(e.path, e.file);
    const res = await apiFetch(url, { method: 'POST', body: formData });
    if (!res.ok) throw await refusal(res);
    onProgress?.(total, total);
    return;
  }

  let done = 0;
  for (const e of entries) {
    const res = await uploadFileChunked(url, e.file, {
      name: e.path,
      onProgress: (sent) => onProgress?.(done + sent, total),
    });
    if (!res.ok) throw await refusal(res);
    done += e.file.size;
  }
}
