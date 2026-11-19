// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

// Chunked uploads let the web UI send a file larger than a reverse proxy in
// front of the hub accepts in one request. The file goes up in requests of
// at most server.hub.uploads.chunk_size bytes, through the same upload route
// as a one-request upload (chat attachments, project workspace files, shared
// directory files), selected by the query parameter upload=<op>:
//
//	POST <route>?upload=start            {"name","size","sha256"?} -> 201 {"uploadId","chunkSize","maxSize","received"}
//	POST <route>?upload=chunk&id=&offset= raw bytes (application/octet-stream) -> 200 {"received"}
//	POST <route>?upload=finish&id=       -> the response of a one-request upload of the file
//	POST <route>?upload=abort&id=        -> 204
//
// Every call passes the route's own authorization (and, for workspace
// routes, its write gate) before it gets here, as a one-request upload does.
// An upload is also bound to the principal that started it and to the
// route's scope (kind, project, directory): any other caller, or the same
// caller on another project's route, gets 404 for its id.
//
// Chunks are appended to a temporary file on disk, never held in memory.
// A chunk must start at the offset received so far (409 with "received"
// otherwise), so a client that lost a response resumes from the offset the
// hub reports. Bytes past the declared size, which start checked against the
// per-file limit, are refused with 413 as they arrive. A chunk that fails
// midway is dropped whole, so it can be sent again.
//
// finish hands the assembled file to the code that stores a one-request
// upload (storeAttachmentParts, writeWorkspaceFile), so storage and
// authorization are unchanged. Its response is kept for
// chunkedUploadResultTTL, so a client whose finish response was lost (a
// proxy timing out while the hub copies a large file) can repeat the finish
// and get the same answer instead of a 404. Uploads idle for
// chunkedUploadIdleTTL are removed by the chunked-upload-sweep scheduler task.
//
// Capacity: the declared sizes of all uploads in progress are capped by
// server.hub.uploads.max_pending_size, and one caller's by a quarter of it
// (at least the route's per-file limit), so one caller cannot take it all.
//
// Uploads live in this instance's memory and temporary directory, so every
// call of one upload must reach the same hub instance.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// chunkedUploadOpParam is the query parameter that selects a chunked upload
// operation on an upload route.
const chunkedUploadOpParam = "upload"

const (
	// chunkedUploadIdleTTL is how long an upload may go without a call
	// before the sweep removes it.
	chunkedUploadIdleTTL = time.Hour
	// chunkedUploadMaxPerOwner caps the uploads one principal has in
	// progress at once.
	chunkedUploadMaxPerOwner = 4
	// chunkedUploadStartBodyMax bounds the JSON body of a start call.
	chunkedUploadStartBodyMax = 64 << 10
	// chunkedUploadChunkReadBudget is the read deadline of one chunk call
	// when the server's ReadTimeout is shorter, so a chunk on a slow link
	// is not cut at the listener's ReadTimeout.
	chunkedUploadChunkReadBudget = 5 * time.Minute
	// chunkedUploadFinishWriteBudget is the write deadline of a finish call,
	// which copies the whole file to its storage before it answers.
	chunkedUploadFinishWriteBudget = 15 * time.Minute
	// chunkedUploadStaleDirAge is the age past which a temporary directory
	// left by an earlier hub process is removed.
	chunkedUploadStaleDirAge = 24 * time.Hour
	// chunkedUploadDirPrefix names the per-process temporary directory.
	chunkedUploadDirPrefix = "scion-uploads-"
	// chunkedUploadResultTTL is how long the response of a finished upload
	// is kept for a repeated finish call.
	chunkedUploadResultTTL = 15 * time.Minute
	// chunkedUploadResultMax bounds the response body kept for a repeated
	// finish call.
	chunkedUploadResultMax = 64 << 10
	// chunkedUploadMaxResults bounds the responses kept; the oldest is
	// dropped first.
	chunkedUploadMaxResults = 1024
	// chunkedUploadAbandonedAfter is how long one caller's upload may go
	// without a call before a new upload of the same caller, refused
	// otherwise by the caller's limits, replaces it (a tab closed mid-upload
	// sends no abort).
	chunkedUploadAbandonedAfter = 10 * time.Minute
)

var (
	errChunkedUploadNotFound   = errors.New("upload not found")
	errChunkedUploadBusy       = errors.New("another call of this upload is in progress")
	errChunkedUploadTooMany    = errors.New("too many uploads in progress")
	errChunkedUploadNoCapacity = errors.New("upload capacity reached")
	errChunkedUploadOwnerQuota = errors.New("caller's upload capacity reached")
	errChunkPastDeclaredSize   = errors.New("upload exceeds its declared size")
)

// uploadPart is one uploaded file, from a multipart form or a finished
// chunked upload.
type uploadPart struct {
	Filename string
	Size     int64
	Open     func() (multipart.File, error)
}

// partFromHeader returns the uploadPart of a multipart file.
func partFromHeader(fh *multipart.FileHeader) uploadPart {
	return uploadPart{Filename: fh.Filename, Size: fh.Size, Open: fh.Open}
}

// chunkedUploadFile is the assembled file of a finished chunked upload. Its
// temporary file at path is removed after finish returns.
type chunkedUploadFile struct {
	name string
	size int64
	path string
}

// part returns f as an uploadPart.
func (f chunkedUploadFile) part() uploadPart {
	return uploadPart{
		Filename: f.name,
		Size:     f.size,
		Open:     func() (multipart.File, error) { return os.Open(f.path) },
	}
}

// chunkedUploadTarget is what an upload route passes for a chunked upload.
type chunkedUploadTarget struct {
	// scope names the route's target (kind, project, directory). An upload
	// can be continued and finished only on a route with the same scope.
	scope string
	// maxSize is the per-file limit.
	maxSize int64
	// validateName checks the file name at start, so a name the route
	// would refuse is refused before any byte is sent.
	validateName func(string) error
	// finish stores the assembled file and writes the response.
	finish func(w http.ResponseWriter, r *http.Request, f chunkedUploadFile)
}

// chunkedUploads holds the chunked uploads in progress on this instance.
// The zero value is ready to use.
type chunkedUploads struct {
	mu       sync.Mutex
	baseDir  string // parent of dir; "" means os.TempDir()
	dir      string // created on first use
	sessions map[string]*chunkedUpload
	reserved int64 // sum of the declared sizes of sessions
	finished map[string]*chunkedUploadResult
}

// chunkedUploadResult is the response of a finished upload, kept for a
// repeated finish call of the same owner and scope.
type chunkedUploadResult struct {
	owner, scope string
	status       int
	contentType  string
	body         []byte
	expires      time.Time
}

// chunkedUpload is one upload in progress. received is changed only by the
// call that holds busy, and under mu.
type chunkedUpload struct {
	id, owner, scope, name, sha256, path string
	size, received                       int64
	lastUsed                             time.Time
	busy                                 bool
}

// begin records a new upload of size bytes and creates its empty temporary
// file. maxPending caps the declared sizes of all uploads in progress, and
// ownerMax those of owner's.
func (m *chunkedUploads) begin(owner, scope, name, sum string, size, maxPending, ownerMax int64, now time.Time) (*chunkedUpload, error) {
	m.mu.Lock()
	expired := m.expireLocked(now)
	defer func() {
		m.mu.Unlock()
		removeUploadFiles(expired)
	}()
	n, ownerReserved := m.ownerUsageLocked(owner)
	if n >= chunkedUploadMaxPerOwner || ownerReserved+size > ownerMax {
		// Replace this caller's abandoned uploads before refusing.
		for id, u := range m.sessions {
			if u.owner == owner && !u.busy && now.Sub(u.lastUsed) > chunkedUploadAbandonedAfter {
				delete(m.sessions, id)
				m.reserved -= u.size
				expired = append(expired, u.path)
			}
		}
		n, ownerReserved = m.ownerUsageLocked(owner)
	}
	if n >= chunkedUploadMaxPerOwner {
		return nil, errChunkedUploadTooMany
	}
	if ownerReserved+size > ownerMax {
		return nil, errChunkedUploadOwnerQuota
	}
	if m.reserved+size > maxPending {
		return nil, errChunkedUploadNoCapacity
	}
	dir, err := m.dirLocked(now)
	if err != nil {
		return nil, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(raw[:])
	path := filepath.Join(dir, id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	u := &chunkedUpload{id: id, owner: owner, scope: scope, name: name, sha256: sum,
		path: path, size: size, lastUsed: now}
	if m.sessions == nil {
		m.sessions = map[string]*chunkedUpload{}
	}
	m.sessions[id] = u
	m.reserved += size
	return u, nil
}

// ownerUsageLocked returns the number and the declared sizes of owner's
// uploads in progress.
func (m *chunkedUploads) ownerUsageLocked(owner string) (int, int64) {
	n := 0
	var size int64
	for _, u := range m.sessions {
		if u.owner == owner {
			n++
			size += u.size
		}
	}
	return n, size
}

// dirLocked returns the temporary directory, creating it (and removing
// stale directories of earlier processes) on first use or after something
// else removed it.
func (m *chunkedUploads) dirLocked(now time.Time) (string, error) {
	if m.dir != "" {
		if _, err := os.Stat(m.dir); err == nil {
			return m.dir, nil
		}
	}
	base := m.baseDir
	if base == "" {
		base = os.TempDir()
	}
	removeStaleUploadDirs(base, now)
	dir, err := os.MkdirTemp(base, chunkedUploadDirPrefix)
	if err != nil {
		return "", err
	}
	m.dir = dir
	return dir, nil
}

// removeStaleUploadDirs removes upload directories under base not modified
// for chunkedUploadStaleDirAge: left by an earlier process, since an upload
// idle for chunkedUploadIdleTTL is removed by its own process.
func removeStaleUploadDirs(base string, now time.Time) {
	matches, _ := filepath.Glob(filepath.Join(base, chunkedUploadDirPrefix+"*"))
	for _, d := range matches {
		if fi, err := os.Lstat(d); err == nil && fi.IsDir() && now.Sub(fi.ModTime()) > chunkedUploadStaleDirAge {
			_ = os.RemoveAll(d)
		}
	}
}

// acquire returns the upload id of owner on scope, marked busy for one call.
// An unknown id, or one of another owner or scope, is errChunkedUploadNotFound.
func (m *chunkedUploads) acquire(id, owner, scope string, now time.Time) (*chunkedUpload, error) {
	u, res, err := m.acquireOrResult(id, owner, scope, now)
	if res != nil {
		// Finished: only a repeated finish (acquireOrResult) gets its answer.
		return nil, errChunkedUploadNotFound
	}
	return u, err
}

// acquireOrResult is acquire that, under the same lock, returns instead the
// kept response when the upload has already been finished, so a repeated
// finish racing the first one's removal never sees neither.
func (m *chunkedUploads) acquireOrResult(id, owner, scope string, now time.Time) (*chunkedUpload, *chunkedUploadResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if res := m.finished[id]; res != nil && res.owner == owner && res.scope == scope && !now.After(res.expires) {
		return nil, res, nil
	}
	u := m.sessions[id]
	if u == nil || u.owner != owner || u.scope != scope {
		return nil, nil, errChunkedUploadNotFound
	}
	if u.busy {
		return nil, nil, errChunkedUploadBusy
	}
	u.busy = true
	u.lastUsed = now
	return u, nil, nil
}

// release ends a call of u, recording the bytes received.
func (m *chunkedUploads) release(u *chunkedUpload, received int64, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u.received = received
	u.busy = false
	u.lastUsed = now
}

// finish records the response of finished upload u for a repeated finish
// call. remove must still be called.
func (m *chunkedUploads) finish(u *chunkedUpload, status int, contentType string, body []byte, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finished == nil {
		m.finished = map[string]*chunkedUploadResult{}
	}
	if len(m.finished) >= chunkedUploadMaxResults {
		oldest := ""
		for id, res := range m.finished {
			if oldest == "" || res.expires.Before(m.finished[oldest].expires) {
				oldest = id
			}
		}
		delete(m.finished, oldest)
	}
	m.finished[u.id] = &chunkedUploadResult{owner: u.owner, scope: u.scope, status: status,
		contentType: contentType, body: body, expires: now.Add(chunkedUploadResultTTL)}
}

// remove deletes u and its temporary file.
func (m *chunkedUploads) remove(u *chunkedUpload) {
	m.mu.Lock()
	if m.sessions[u.id] == u {
		delete(m.sessions, u.id)
		m.reserved -= u.size
	}
	m.mu.Unlock()
	_ = os.Remove(u.path)
}

// sweep removes the uploads idle for chunkedUploadIdleTTL.
func (m *chunkedUploads) sweep(now time.Time) {
	m.mu.Lock()
	expired := m.expireLocked(now)
	m.mu.Unlock()
	removeUploadFiles(expired)
}

// expireLocked drops the idle uploads from the map, and the kept results
// past their TTL, and returns the uploads' files.
func (m *chunkedUploads) expireLocked(now time.Time) []string {
	for id, res := range m.finished {
		if now.After(res.expires) {
			delete(m.finished, id)
		}
	}
	var paths []string
	for id, u := range m.sessions {
		if !u.busy && now.Sub(u.lastUsed) > chunkedUploadIdleTTL {
			delete(m.sessions, id)
			m.reserved -= u.size
			paths = append(paths, u.path)
		}
	}
	if len(paths) > 0 {
		slog.Info("Removed idle chunked uploads", "count", len(paths))
	}
	return paths
}

func removeUploadFiles(paths []string) {
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

// chunkedUploadOwner names the caller an upload is bound to: "user:" and
// the user ID for a user (whether signed in or using a token, so both share
// one quota), otherwise the identity type and ID, so a user and an agent with
// the same ID never match.
func chunkedUploadOwner(ctx context.Context) (string, bool) {
	if user := GetUserIdentityFromContext(ctx); user != nil && user.ID() != "" {
		return "user:" + user.ID(), true
	}
	id := GetIdentityFromContext(ctx)
	if id == nil || id.ID() == "" {
		return "", false
	}
	return fmt.Sprintf("%T:%s", id, id.ID()), true
}

// serveChunkedUpload runs one chunked upload operation for t. The caller
// has authorized the request for an upload to t.
func (s *Server) serveChunkedUpload(w http.ResponseWriter, r *http.Request, op string, t chunkedUploadTarget) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	owner, ok := chunkedUploadOwner(r.Context())
	if !ok {
		Forbidden(w)
		return
	}
	switch op {
	case "start":
		s.startChunkedUpload(w, r, owner, t)
	case "chunk":
		s.writeUploadChunk(w, r, owner, t)
	case "finish":
		s.finishChunkedUpload(w, r, owner, t)
	case "abort":
		s.abortChunkedUpload(w, r, owner, t)
	default:
		BadRequest(w, "unknown upload operation")
	}
}

// chunkedUploadStartRequest is the body of a start call.
type chunkedUploadStartRequest struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	// SHA256 is the optional hex SHA-256 of the whole file, checked at
	// finish.
	SHA256 string `json:"sha256,omitempty"`
}

// chunkedUploadStartResponse answers a start call.
type chunkedUploadStartResponse struct {
	UploadID  string `json:"uploadId"`
	ChunkSize int64  `json:"chunkSize"`
	MaxSize   int64  `json:"maxSize"`
	Received  int64  `json:"received"`
}

func (s *Server) startChunkedUpload(w http.ResponseWriter, r *http.Request, owner string, t chunkedUploadTarget) {
	r.Body = http.MaxBytesReader(w, r.Body, chunkedUploadStartBodyMax)
	var req chunkedUploadStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}
	if req.Size < 0 {
		ValidationError(w, "size must not be negative", nil)
		return
	}
	if req.Size > t.maxSize {
		writeFileTooLarge(w, req.Name, t.maxSize)
		return
	}
	if err := t.validateName(req.Name); err != nil {
		BadRequest(w, fmt.Sprintf("Invalid file name %q: %s", req.Name, err.Error()))
		return
	}
	sum := strings.ToLower(req.SHA256)
	if sum != "" {
		if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
			ValidationError(w, "sha256 must be 64 hexadecimal digits", nil)
			return
		}
	}
	maxPending := s.config.Uploads.EffectiveMaxPendingSize()
	ownerMax := min(maxPending, max(maxPending/4, t.maxSize))
	if req.Size > ownerMax {
		// Only when the route's limit is above the pending cap (an artifact
		// service limit): such a file could never start.
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("File %q exceeds the hub's capacity for uploads in progress (%d bytes)", req.Name, ownerMax),
			map[string]interface{}{"maxSize": ownerMax})
		return
	}
	u, err := s.chunkedUploads.begin(owner, t.scope, req.Name, sum, req.Size, maxPending, ownerMax, time.Now())
	switch {
	case errors.Is(err, errChunkedUploadOwnerQuota):
		writeError(w, http.StatusTooManyRequests, "too_many_uploads",
			"Your uploads in progress already hold as much as one caller may; wait for one to finish", nil)
		return
	case errors.Is(err, errChunkedUploadTooMany):
		writeError(w, http.StatusTooManyRequests, "too_many_uploads",
			fmt.Sprintf("Too many uploads in progress (at most %d); wait for one to finish", chunkedUploadMaxPerOwner), nil)
		return
	case errors.Is(err, errChunkedUploadNoCapacity):
		writeError(w, http.StatusServiceUnavailable, "upload_capacity_reached",
			"The hub is holding as many uploads in progress as it allows; try again later", nil)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "Failed to start chunked upload", "error", err)
		InternalError(w)
		return
	}
	writeJSON(w, http.StatusCreated, chunkedUploadStartResponse{
		UploadID:  u.id,
		ChunkSize: s.config.Uploads.EffectiveChunkSize(),
		MaxSize:   t.maxSize,
	})
}

// acquireChunkedUpload looks up the upload named by the id query parameter
// and writes the refusal when there is none.
func (s *Server) acquireChunkedUpload(w http.ResponseWriter, r *http.Request, owner string, t chunkedUploadTarget) (*chunkedUpload, bool) {
	u, err := s.chunkedUploads.acquire(r.URL.Query().Get("id"), owner, t.scope, time.Now())
	switch {
	case errors.Is(err, errChunkedUploadBusy):
		writeError(w, http.StatusConflict, "upload_busy", err.Error(), nil)
		return nil, false
	case err != nil:
		NotFound(w, "Upload")
		return nil, false
	}
	return u, true
}

// writeUploadReceived answers a call whose offset or completeness does not
// match the bytes received so far, naming them so the client can resume.
func writeUploadReceived(w http.ResponseWriter, code, msg string, received int64) {
	writeError(w, http.StatusConflict, code, msg, map[string]interface{}{"received": received})
}

// recordingReader remembers the first read error of r other than io.EOF,
// so a failed copy can tell a broken request from a failed disk write.
type recordingReader struct {
	r   io.Reader
	err error
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if err != nil && err != io.EOF && rr.err == nil {
		rr.err = err
	}
	return n, err
}

func (s *Server) writeUploadChunk(w http.ResponseWriter, r *http.Request, owner string, t chunkedUploadTarget) {
	u, ok := s.acquireChunkedUpload(w, r, owner, t)
	if !ok {
		return
	}
	received := u.received
	defer func() { s.chunkedUploads.release(u, received, time.Now()) }()

	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		BadRequest(w, "invalid offset")
		return
	}
	if offset != received {
		writeUploadReceived(w, "upload_offset_mismatch",
			fmt.Sprintf("Chunk offset %d does not match the %d bytes received", offset, received), received)
		return
	}

	extendReadDeadline(r.Context(), w, chunkedUploadChunkReadBudget)
	extendWriteDeadline(r.Context(), w, s.config.WriteTimeout, chunkedUploadChunkReadBudget+time.Minute)
	chunkSize := s.config.Uploads.EffectiveChunkSize()
	body := &recordingReader{r: http.MaxBytesReader(w, r.Body, chunkSize)}
	n, err := appendUploadChunk(u.path, received, body, u.size-received)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("Chunk exceeds the chunk size of %d bytes", chunkSize), map[string]interface{}{"chunkSize": chunkSize})
		return
	case errors.Is(err, errChunkPastDeclaredSize):
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			fmt.Sprintf("Upload exceeds its declared size of %d bytes", u.size), map[string]interface{}{"received": received})
		return
	case err != nil && body.err != nil:
		BadRequest(w, "Upload chunk interrupted; send it again")
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "Failed to write upload chunk", "error", err)
		InternalError(w)
		return
	}
	received += n
	writeJSON(w, http.StatusOK, map[string]int64{"received": received})
}

// appendUploadChunk writes body at offset of the file at path and returns
// the bytes written. Bytes past remaining are refused with
// errChunkPastDeclaredSize. On any error the file is cut back to offset, so
// the chunk can be sent again whole.
func appendUploadChunk(path string, offset int64, body io.Reader, remaining int64) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	n, err := func() (int64, error) {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, err
		}
		n, err := io.Copy(f, io.LimitReader(body, remaining+1))
		if err == nil && n > remaining {
			err = errChunkPastDeclaredSize
		}
		return n, err
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Truncate(path, offset)
		return 0, err
	}
	return n, nil
}

func (s *Server) finishChunkedUpload(w http.ResponseWriter, r *http.Request, owner string, t chunkedUploadTarget) {
	u, res, err := s.chunkedUploads.acquireOrResult(r.URL.Query().Get("id"), owner, t.scope, time.Now())
	switch {
	case res != nil:
		// A repeated finish of an upload already finished gets the same answer.
		if res.contentType != "" {
			w.Header().Set("Content-Type", res.contentType)
		}
		w.WriteHeader(res.status)
		_, _ = w.Write(res.body)
		return
	case errors.Is(err, errChunkedUploadBusy):
		writeError(w, http.StatusConflict, "upload_busy", err.Error(), nil)
		return
	case err != nil:
		NotFound(w, "Upload")
		return
	}
	if u.received != u.size {
		s.chunkedUploads.release(u, u.received, time.Now())
		writeUploadReceived(w, "upload_incomplete",
			fmt.Sprintf("Upload has %d of its %d bytes", u.received, u.size), u.received)
		return
	}
	// From here the upload is consumed whatever the outcome, and the answer
	// is kept for a repeated finish.
	rec := &resultRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if !rec.overflow {
			s.chunkedUploads.finish(u, rec.status, rec.Header().Get("Content-Type"), rec.body.Bytes(), time.Now())
		}
		s.chunkedUploads.remove(u)
	}()
	extendWriteDeadline(r.Context(), rec, s.config.WriteTimeout, chunkedUploadFinishWriteBudget)
	if u.sha256 != "" {
		got, err := fileSHA256(u.path)
		if err != nil {
			slog.ErrorContext(r.Context(), "Failed to hash chunked upload", "error", err)
			InternalError(rec)
			return
		}
		if got != u.sha256 {
			writeError(rec, http.StatusUnprocessableEntity, "checksum_mismatch",
				"The uploaded file does not match its sha256", nil)
			return
		}
	}
	t.finish(rec, r, chunkedUploadFile{name: u.name, size: u.size, path: u.path})
}

// resultRecorder passes a response through and keeps its status and body
// (up to chunkedUploadResultMax) for a repeated finish call.
type resultRecorder struct {
	http.ResponseWriter
	status   int
	wrote    bool
	body     bytes.Buffer
	overflow bool
}

func (rr *resultRecorder) WriteHeader(status int) {
	if !rr.wrote {
		rr.status = status
		rr.wrote = true
	}
	rr.ResponseWriter.WriteHeader(status)
}

func (rr *resultRecorder) Write(p []byte) (int, error) {
	rr.wrote = true
	if !rr.overflow {
		if rr.body.Len()+len(p) > chunkedUploadResultMax {
			rr.overflow = true
			rr.body.Reset()
		} else {
			rr.body.Write(p)
		}
	}
	return rr.ResponseWriter.Write(p)
}

// Unwrap lets http.NewResponseController reach the connection.
func (rr *resultRecorder) Unwrap() http.ResponseWriter { return rr.ResponseWriter }

func (s *Server) abortChunkedUpload(w http.ResponseWriter, r *http.Request, owner string, t chunkedUploadTarget) {
	u, ok := s.acquireChunkedUpload(w, r, owner, t)
	if !ok {
		return
	}
	s.chunkedUploads.remove(u)
	w.WriteHeader(http.StatusNoContent)
}

// fileSHA256 returns the hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extendReadDeadline moves the connection's read deadline to budget from
// now when the serving listener's ReadTimeout is positive and shorter, so
// it never shortens it (see extendWriteDeadline).
func extendReadDeadline(ctx context.Context, w http.ResponseWriter, budget time.Duration) {
	hs, ok := ctx.Value(http.ServerContextKey).(*http.Server)
	if !ok || hs == nil || hs.ReadTimeout <= 0 || hs.ReadTimeout >= budget {
		return
	}
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(budget)); err != nil {
		slog.DebugContext(ctx, "chunked upload: SetReadDeadline not applied", "error", err)
	}
}
