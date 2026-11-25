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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/require"
)

// testChunkSize is the smallest chunk size the setting allows.
const testChunkSize = config.MinUploadChunkSize

// chunkedCall posts one chunked upload operation to target (an upload route
// without query string). query is appended as is.
func chunkedCall(t *testing.T, srv *Server, target, op, query string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = http.NoBody
	}
	req := httptest.NewRequest(http.MethodPost, target+"?upload="+op+query, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// startChunked starts an upload of name and size (sum optional) and returns
// its id.
func startChunked(t *testing.T, srv *Server, target, extraQuery, name string, size int64, sum string) string {
	t.Helper()
	body, err := json.Marshal(chunkedUploadStartRequest{Name: name, Size: size, SHA256: sum})
	require.NoError(t, err)
	rec := chunkedCall(t, srv, target, "start", extraQuery, bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp chunkedUploadStartResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.UploadID)
	require.Equal(t, srv.config.Uploads.EffectiveChunkSize(), resp.ChunkSize)
	return resp.UploadID
}

// sendChunk posts data as the chunk of upload id at offset.
func sendChunk(t *testing.T, srv *Server, target, extraQuery, id string, offset int64, data io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	return chunkedCall(t, srv, target, "chunk",
		extraQuery+"&id="+id+"&offset="+strconv.FormatInt(offset, 10), data, "application/octet-stream")
}

// uploadAllChunks sends content in chunks of the configured size and fails
// on any refusal.
func uploadAllChunks(t *testing.T, srv *Server, target, extraQuery, id string, content []byte) {
	t.Helper()
	uploadChunksFrom(t, srv, target, extraQuery, id, content, 0)
}

// uploadChunksFrom is uploadAllChunks from offset from.
func uploadChunksFrom(t *testing.T, srv *Server, target, extraQuery, id string, content []byte, from int) {
	t.Helper()
	chunk := int(srv.config.Uploads.EffectiveChunkSize())
	for off := from; off < len(content); off += chunk {
		end := min(off+chunk, len(content))
		rec := sendChunk(t, srv, target, extraQuery, id, int64(off), bytes.NewReader(content[off:end]))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, float64(end), decodeJSONMap(t, rec)["received"])
	}
}

func decodeJSONMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m), rec.Body.String())
	return m
}

// errorDetail returns details[key] of an error response.
func errorDetail(t *testing.T, rec *httptest.ResponseRecorder, key string) interface{} {
	t.Helper()
	m := decodeJSONMap(t, rec)
	e, _ := m["error"].(map[string]interface{})
	d, _ := e["details"].(map[string]interface{})
	return d[key]
}

// uploadTempFiles lists the files in the chunked upload directories under dir.
func uploadTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, chunkedUploadDirPrefix+"*", "*"))
	require.NoError(t, err)
	return matches
}

func randomContent(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

// chunkedWorkspaceServer returns a server with a hub-managed project, small
// chunks and the given workspace per-file limit, its upload route and
// workspace path, and the temporary directory uploads spool to.
func chunkedWorkspaceServer(t *testing.T, maxFile int64) (*Server, string, string, string) {
	t.Helper()
	srv, _ := testServer(t)
	srv.config.Uploads = config.HubUploadsConfig{ChunkSize: testChunkSize, MaxWorkspaceFileSize: maxFile}
	project, workspacePath := createTestHubManagedProject(t, srv, "Chunked "+t.Name())
	tmp := redirectMultipartTemp(t)
	return srv, fmt.Sprintf("/api/v1/projects/%s/workspace/files", project.ID), workspacePath, tmp
}

// A file larger than one chunk is assembled byte for byte, its parts are
// held in a temporary file on disk (not in memory) while it is in progress,
// and the temporary file is removed when it is finished.
func TestChunkedWorkspaceUpload_AssemblesFileFromDisk(t *testing.T) {
	srv, target, workspacePath, tmp := chunkedWorkspaceServer(t, 4<<20)
	content := randomContent(int(3*testChunkSize + 1234))
	sum := sha256.Sum256(content)
	id := startChunked(t, srv, target, "", "data/big.bin", int64(len(content)), hex.EncodeToString(sum[:]))

	rec := sendChunk(t, srv, target, "", id, 0, bytes.NewReader(content[:testChunkSize]))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	files := uploadTempFiles(t, tmp)
	require.Len(t, files, 1, "the received part is on disk under TMPDIR")
	fi, err := os.Stat(files[0])
	require.NoError(t, err)
	require.Equal(t, testChunkSize, fi.Size())

	uploadChunksFrom(t, srv, target, "", id, content, int(testChunkSize))
	rec = chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ProjectWorkspaceUploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Files, 1)
	require.Equal(t, "data/big.bin", resp.Files[0].Path)
	require.Equal(t, int64(len(content)), resp.Files[0].Size)

	got, err := os.ReadFile(filepath.Join(workspacePath, "data", "big.bin"))
	require.NoError(t, err)
	require.True(t, bytes.Equal(content, got), "assembled file differs from the input")
	require.Empty(t, uploadTempFiles(t, tmp), "temporary file removed after finish")
	require.Zero(t, srv.chunkedUploads.reserved)

	// The upload is consumed: a repeated finish replays the answer, and
	// a chunk call no longer finds it.
	again := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	rec = sendChunk(t, srv, target, "", id, int64(len(content)), strings.NewReader("x"))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// A chunk that does not start at the bytes received so far is refused with
// 409, naming the offset to resume from.
func TestChunkedUpload_OffsetMismatchRefused(t *testing.T) {
	srv, target, _, _ := chunkedWorkspaceServer(t, 4<<20)
	content := randomContent(int(2 * testChunkSize))
	id := startChunked(t, srv, target, "", "a.bin", int64(len(content)), "")

	rec := sendChunk(t, srv, target, "", id, 10, bytes.NewReader(content[:testChunkSize]))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, float64(0), errorDetail(t, rec, "received"))

	rec = sendChunk(t, srv, target, "", id, 0, bytes.NewReader(content[:testChunkSize]))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// The same chunk again (a client that lost the response) is refused, and
	// the hub says where to continue.
	rec = sendChunk(t, srv, target, "", id, 0, bytes.NewReader(content[:testChunkSize]))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, float64(testChunkSize), errorDetail(t, rec, "received"))

	rec = chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusConflict, rec.Code, "an incomplete upload cannot be finished")
	require.Equal(t, float64(testChunkSize), errorDetail(t, rec, "received"))
}

// The per-file limit is checked at start (413), bytes past the declared
// size are refused as they arrive (413), and so is a chunk larger than the
// chunk size.
func TestChunkedUpload_OverLimitRefused(t *testing.T) {
	srv, target, _, tmp := chunkedWorkspaceServer(t, 1<<20)

	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "huge.bin", Size: 1<<20 + 1})
	rec := chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "1048576 bytes")
	require.Equal(t, float64(1<<20), errorDetail(t, rec, "maxSize"))

	id := startChunked(t, srv, target, "", "small.bin", 10, "")
	rec = sendChunk(t, srv, target, "", id, 0, bytes.NewReader(make([]byte, 11)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	files := uploadTempFiles(t, tmp)
	require.Len(t, files, 1)
	fi, err := os.Stat(files[0])
	require.NoError(t, err)
	require.Zero(t, fi.Size(), "the refused chunk is dropped")

	id2 := startChunked(t, srv, target, "", "medium.bin", 1<<20, "")
	rec = sendChunk(t, srv, target, "", id2, 0, bytes.NewReader(make([]byte, testChunkSize+1)))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "chunk size")
}

// An upload is bound to its route's scope and to the principal that started
// it: another project's route, or another principal, gets 404.
func TestChunkedUpload_BoundToScopeAndOwner(t *testing.T) {
	srv, targetA, _, _ := chunkedWorkspaceServer(t, 4<<20)
	projectB, _ := createTestHubManagedProject(t, srv, "Chunked other project")
	targetB := fmt.Sprintf("/api/v1/projects/%s/workspace/files", projectB.ID)

	id := startChunked(t, srv, targetA, "", "a.txt", 4, "")
	rec := sendChunk(t, srv, targetB, "", id, 0, strings.NewReader("abcd"))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = chunkedCall(t, srv, targetB, "abort", "&id="+id, nil, "")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	_, err := srv.chunkedUploads.acquire(id, "*hub.AuthenticatedUser:someone-else", "workspace:"+strings.Split(targetA, "/")[4], time.Now())
	require.ErrorIs(t, err, errChunkedUploadNotFound)

	// The owner on the right route still can.
	rec = sendChunk(t, srv, targetA, "", id, 0, strings.NewReader("abcd"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// droppedBodyReader returns data, then a non-EOF error, like a request body
// whose connection dropped.
type droppedBodyReader struct{ data io.Reader }

func (f *droppedBodyReader) Read(p []byte) (int, error) {
	n, err := f.data.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errors.New("connection reset")
	}
	return n, err
}

// A chunk that fails midway is dropped whole, so the client can send it
// again from the same offset and the file is still byte-identical.
func TestChunkedUpload_ResumeAfterFailedChunk(t *testing.T) {
	srv, target, workspacePath, tmp := chunkedWorkspaceServer(t, 4<<20)
	content := randomContent(int(testChunkSize + 100))
	id := startChunked(t, srv, target, "", "r.bin", int64(len(content)), "")

	rec := sendChunk(t, srv, target, "", id, 0, &droppedBodyReader{data: bytes.NewReader(content[:testChunkSize/2])})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	files := uploadTempFiles(t, tmp)
	require.Len(t, files, 1)
	fi, err := os.Stat(files[0])
	require.NoError(t, err)
	require.Zero(t, fi.Size(), "the partial chunk is cut back")

	uploadAllChunks(t, srv, target, "", id, content)
	rec = chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := os.ReadFile(filepath.Join(workspacePath, "r.bin"))
	require.NoError(t, err)
	require.True(t, bytes.Equal(content, got))
}

// A file whose bytes do not match the sha256 given at start is refused and
// the upload removed.
func TestChunkedUpload_ChecksumMismatch(t *testing.T) {
	srv, target, workspacePath, tmp := chunkedWorkspaceServer(t, 4<<20)
	wrong := sha256.Sum256([]byte("something else"))
	id := startChunked(t, srv, target, "", "c.txt", 4, hex.EncodeToString(wrong[:]))
	uploadAllChunks(t, srv, target, "", id, []byte("abcd"))
	rec := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Empty(t, uploadTempFiles(t, tmp))
	again := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusUnprocessableEntity, again.Code, "a repeated finish replays the refusal")
	_, err := os.Stat(filepath.Join(workspacePath, "c.txt"))
	require.True(t, os.IsNotExist(err), "nothing written to the workspace")
}

// The sweep removes an upload idle past chunkedUploadIdleTTL with its
// temporary file, and leaves a recent one alone.
func TestChunkedUpload_IdleUploadsSwept(t *testing.T) {
	srv, target, _, tmp := chunkedWorkspaceServer(t, 4<<20)
	id := startChunked(t, srv, target, "", "idle.bin", 8, "")
	require.Len(t, uploadTempFiles(t, tmp), 1)

	srv.chunkedUploads.sweep(time.Now().Add(chunkedUploadIdleTTL / 2))
	require.Len(t, uploadTempFiles(t, tmp), 1, "a recent upload is kept")

	srv.chunkedUploads.sweep(time.Now().Add(chunkedUploadIdleTTL + time.Minute))
	require.Empty(t, uploadTempFiles(t, tmp))
	require.Zero(t, srv.chunkedUploads.reserved)
	rec := sendChunk(t, srv, target, "", id, 0, strings.NewReader("12345678"))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// One principal may hold chunkedUploadMaxPerOwner uploads at once, and the
// declared sizes of all uploads are capped by max_pending_size.
func TestChunkedUpload_Caps(t *testing.T) {
	srv, target, _, _ := chunkedWorkspaceServer(t, 4<<20)
	var ids []string
	for i := 0; i < chunkedUploadMaxPerOwner; i++ {
		ids = append(ids, startChunked(t, srv, target, "", fmt.Sprintf("f%d", i), 1, ""))
	}
	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "one-more", Size: 1})
	rec := chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	for _, id := range ids {
		rec = chunkedCall(t, srv, target, "abort", "&id="+id, nil, "")
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}

	srv.config.Uploads.MaxPendingSize = 4 << 20
	startChunked(t, srv, target, "", "three-mib", 3<<20, "")
	body, _ = json.Marshal(chunkedUploadStartRequest{Name: "two-mib", Size: 2 << 20})
	rec = chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "over this caller's share: %s", rec.Body.String())
	// Another caller within its own share still hits the hub-wide cap.
	_, err := srv.chunkedUploads.begin("user:someone-else", "workspace:x", "x", "", 2<<20, 4<<20, 4<<20, time.Now())
	require.ErrorIs(t, err, errChunkedUploadNoCapacity)
}

// A start names a file the route would refuse: refused before any byte.
func TestChunkedUpload_InvalidNameRefusedAtStart(t *testing.T) {
	srv, target, _, _ := chunkedWorkspaceServer(t, 4<<20)
	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "../escape.txt", Size: 1})
	rec := chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// The workspace per-file limit comes from server.hub.uploads: the default is
// 50 MiB, a larger value is accepted, and a one-request upload over a lower
// value is refused with 413 naming the limit.
func TestWorkspaceUpload_PerFileLimitFromSetting(t *testing.T) {
	srv, target, _, _ := chunkedWorkspaceServer(t, 0)
	require.Equal(t, config.DefaultUploadMaxWorkspaceFileSize, srv.config.Uploads.EffectiveMaxWorkspaceFileSize())
	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "default.bin", Size: config.DefaultUploadMaxWorkspaceFileSize + 1})
	rec := chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())

	srv.config.Uploads.MaxWorkspaceFileSize = 1 << 30
	startChunked(t, srv, target, "", "one-gib.bin", 1<<30, "")

	srv.config.Uploads.MaxWorkspaceFileSize = 1024
	rec = serveUpload(srv, buildMultipartTestRequest(t, http.MethodPost, target, testDevToken,
		multipartTestPart{field: "big.txt", filename: "big.txt", content: bytes.Repeat([]byte("x"), 2048)}))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "1024 bytes")
}

// chunkedChatServer is attachmentTestServer with small chunks and the given
// attachment per-file limit.
func chunkedChatServer(t *testing.T, maxFile int64) *Server {
	t.Helper()
	srv, _ := attachmentTestServer(t)
	srv.config.Uploads = config.HubUploadsConfig{ChunkSize: testChunkSize, MaxAttachmentSize: maxFile}
	redirectMultipartTemp(t)
	return srv
}

// A chat attachment larger than one chunk is stored through the same code
// as a one-request upload, byte-identical, with the same response.
func TestChunkedChatAttachmentUpload(t *testing.T) {
	srv := chunkedChatServer(t, 2<<20)
	const target = "/api/v1/chat/attachments"
	content := bytes.Repeat([]byte("log line of a large text attachment\n"), int(testChunkSize)/36*3)
	id := startChunked(t, srv, target, "", "big.txt", int64(len(content)), "")
	uploadAllChunks(t, srv, target, "", id, content)
	rec := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeUploadResponse(t, rec)
	require.Len(t, resp.Attachments, 1)
	require.Empty(t, resp.Failures)
	require.Equal(t, int64(len(content)), resp.Attachments[0].Size)
	require.Equal(t, "text/plain", resp.Attachments[0].MimeType)

	req := httptest.NewRequest(http.MethodGet, resp.Attachments[0].URL, nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	got := httptest.NewRecorder()
	srv.Handler().ServeHTTP(got, req)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	require.True(t, bytes.Equal(content, got.Body.Bytes()), "stored attachment differs from the input")
}

// The chat per-file limit comes from server.hub.uploads.max_attachment_size,
// for chunked and one-request uploads.
func TestChatAttachmentUpload_PerFileLimitFromSetting(t *testing.T) {
	srv := chunkedChatServer(t, 1024)
	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "big.txt", Size: 1025})
	rec := chunkedCall(t, srv, "/api/v1/chat/attachments", "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())

	rec = uploadAttachments(t, srv, []uploadFile{{name: "big.txt", mime: "text/plain", content: strings.Repeat("x", 2048)}})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	resp := decodeUploadResponse(t, rec)
	require.Len(t, resp.Failures, 1)
	require.Contains(t, resp.Failures[0].Error, "1024 bytes")

	// Above the old fixed 10 MiB limit: accepted at start.
	srv.config.Uploads.MaxAttachmentSize = 1 << 30
	startChunked(t, srv, "/api/v1/chat/attachments", "", "huge.log", 1<<30, "")
}

// The web UI reads the limits from the public settings.
func TestPublicSettings_UploadLimits(t *testing.T) {
	srv := chunkedChatServer(t, 3<<20)
	srv.config.Uploads.MaxWorkspaceFileSize = 1 << 30
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/public", nil)
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp PublicSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, PublicUploadLimits{
		ChunkSize:            testChunkSize,
		MaxAttachmentSize:    3 << 20,
		MaxWorkspaceFileSize: 1 << 30,
	}, resp.Uploads)
}

// A chunk's application/octet-stream body passes the proxy-mode write guard
// only as a chunk call on a chunked upload route.
func TestAcceptableWriteBody_OctetStreamOnUploadRoutes(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/v1/chat/attachments?upload=start":                 false,
		"/api/v1/chat/attachments":                              false,
		"/api/v1/templates/t1/files?upload=chunk":               false,
		"/api/v1/projects/p1/workspace/files?upload=chunk&id=x": true,
		"/api/v1/chat/attachments?upload=chunk":                 true,
		"/api/v1/projects/p1/shared-dirs/d/files?upload=chunk":  true,
		"/api/v1/projects?upload=chunk":                         false,
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("x"))
		req.Header.Set("Content-Type", "application/octet-stream")
		require.Equal(t, want, acceptableWriteBody(req), path)
	}
}

// A finish whose response was lost can be repeated: it gets the same answer,
// and the file is stored once.
func TestChunkedUpload_RepeatedFinishGetsSameAnswer(t *testing.T) {
	srv := chunkedChatServer(t, 2<<20)
	const target = "/api/v1/chat/attachments"
	content := []byte("a small text file\n")
	id := startChunked(t, srv, target, "", "repeat.txt", int64(len(content)), "")
	uploadAllChunks(t, srv, target, "", id, content)
	first := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())
	again := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusCreated, again.Code, again.Body.String())
	require.JSONEq(t, first.Body.String(), again.Body.String())

	// Past the TTL the id is gone.
	srv.chunkedUploads.sweep(time.Now().Add(chunkedUploadResultTTL + time.Minute))
	rec := chunkedCall(t, srv, target, "finish", "&id="+id, nil, "")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// One caller may reserve at most a quarter of max_pending_size (at least
// the per-file limit), so it cannot block everyone else's uploads.
func TestChunkedUpload_PerCallerCapacity(t *testing.T) {
	srv, target, _, _ := chunkedWorkspaceServer(t, 1<<20)
	srv.config.Uploads.MaxPendingSize = 8 << 20 // a caller may hold 2 MiB
	startChunked(t, srv, target, "", "a", 1<<20, "")
	startChunked(t, srv, target, "", "b", 1<<20, "")
	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "c", Size: 1})
	rec := chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
}

// Another user cannot continue, finish or cancel an upload, even on a route
// both may use, and a user without access to a project cannot start one in
// its workspace.
func TestChunkedUpload_OtherUserRefused(t *testing.T) {
	srv, s := attachmentTestServer(t)
	srv.config.Uploads = config.HubUploadsConfig{ChunkSize: testChunkSize}
	redirectMultipartTemp(t)
	const chat = "/api/v1/chat/attachments"
	id := startChunked(t, srv, chat, "", "mine.txt", 4, "")

	other := createTestUser(t, s, "chunked-other", "chunked-other@example.com")
	for _, op := range []string{"chunk&offset=0", "finish", "abort"} {
		rec := doRequestAsUser(t, srv, other, http.MethodPost, chat+"?upload="+op+"&id="+id, map[string]string{"x": "y"})
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", op, rec.Body.String())
	}
	// The owner still can.
	rec := sendChunk(t, srv, chat, "", id, 0, strings.NewReader("abcd"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	project, _ := createTestHubManagedProject(t, srv, "Chunked private")
	rec = doRequestAsUser(t, srv, other, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/workspace/files?upload=start", project.ID),
		chunkedUploadStartRequest{Name: "x.bin", Size: 1})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
}

// While chat attachments are stored as artifacts, the public settings
// publish the artifact service's file limit, also to an anonymous caller:
// that is the limit a signed-in web user's upload gets.
func TestPublicSettings_AttachmentLimitFromArtifacts(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	f.srv.config.Uploads.MaxAttachmentSize = 1 << 30
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/public", nil)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp PublicSettingsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, f.srv.attachmentMaxBytes(context.Background(), true), resp.Uploads.MaxAttachmentSize)
	require.NotEqual(t, int64(1<<30), resp.Uploads.MaxAttachmentSize)
}

// An upload of the same caller idle past chunkedUploadIdleTTL (a tab closed
// mid-upload) is removed when the caller's limits would otherwise refuse a
// new one.
func TestChunkedUpload_AbandonedUploadReplaced(t *testing.T) {
	srv, target, _, tmp := chunkedWorkspaceServer(t, 1<<20)
	srv.config.Uploads.MaxPendingSize = 4 << 20 // a caller may hold 1 MiB
	startChunked(t, srv, target, "", "abandoned", 1<<20, "")
	body, _ := json.Marshal(chunkedUploadStartRequest{Name: "next", Size: 1 << 20})
	rec := chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "a recent upload is kept: %s", rec.Body.String())

	srv.chunkedUploads.mu.Lock()
	for _, u := range srv.chunkedUploads.sessions {
		u.lastUsed = time.Now().Add(-chunkedUploadIdleTTL - time.Minute)
	}
	srv.chunkedUploads.mu.Unlock()
	startChunked(t, srv, target, "", "next", 1<<20, "")
	require.Len(t, uploadTempFiles(t, tmp), 1, "the abandoned upload's file is removed")
}

// Uploads that other callers started and then left idle for 10 minutes no
// longer hold the hub-wide capacity: a start that would get 503 removes
// them first, while uploads used more recently are kept.
func TestChunkedUpload_IdleUploadsOfOthersFreeCapacity(t *testing.T) {
	srv, target, _, tmp := chunkedWorkspaceServer(t, 1<<20)
	srv.config.Uploads.MaxPendingSize = 4 << 20
	now := time.Now()
	for i := 0; i < 4; i++ {
		_, err := srv.chunkedUploads.begin(fmt.Sprintf("user:holder-%d", i), "workspace:x", "x", "",
			1<<20, 4<<20, 1<<20, now)
		require.NoError(t, err)
	}
	idleFor := func(d time.Duration) {
		srv.chunkedUploads.mu.Lock()
		defer srv.chunkedUploads.mu.Unlock()
		for _, u := range srv.chunkedUploads.sessions {
			u.lastUsed = time.Now().Add(-d)
		}
	}
	start := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(chunkedUploadStartRequest{Name: "next", Size: 1 << 20})
		return chunkedCall(t, srv, target, "start", "", bytes.NewReader(body), "application/json")
	}

	rec := start()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	idleFor(5 * time.Minute)
	rec = start()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "uploads used 5 minutes ago are kept: %s", rec.Body.String())

	idleFor(10*time.Minute + time.Second)
	rec = start()
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.Len(t, uploadTempFiles(t, tmp), 1, "the idle uploads' files are removed")
	require.Equal(t, int64(1<<20), srv.chunkedUploads.reserved)
}
