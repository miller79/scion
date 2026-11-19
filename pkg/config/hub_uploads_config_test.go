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

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubUploadsConfig_Defaults: an unset block keeps the limits the hub had
// before they were configurable, with 8 MiB chunks and a 1 GiB pending cap.
func TestHubUploadsConfig_Defaults(t *testing.T) {
	var c HubUploadsConfig
	assert.True(t, c.IsZero())
	assert.NoError(t, c.Validate())
	assert.Equal(t, int64(10<<20), c.EffectiveMaxAttachmentSize())
	assert.Equal(t, int64(50<<20), c.EffectiveMaxWorkspaceFileSize())
	assert.Equal(t, int64(8<<20), c.EffectiveChunkSize())
	assert.Equal(t, int64(1<<30), c.EffectiveMaxPendingSize())

	big := HubUploadsConfig{MaxAttachmentSize: 1 << 30, MaxWorkspaceFileSize: 1 << 30}
	assert.NoError(t, big.Validate(), "1 GiB per file is accepted")
	assert.Equal(t, int64(4<<30), big.EffectiveMaxPendingSize(), "default pending cap is four of the larger limit")
}

// TestHubUploadsConfig_Validate refuses negative values, per-file limits
// above MaxUploadFileSize, chunk sizes out of range and a pending cap below
// a per-file limit.
func TestHubUploadsConfig_Validate(t *testing.T) {
	for _, ok := range []HubUploadsConfig{
		{ChunkSize: MinUploadChunkSize},
		{ChunkSize: MaxUploadChunkSize},
		{MaxWorkspaceFileSize: MaxUploadFileSize, MaxPendingSize: MaxUploadFileSize},
		{MaxAttachmentSize: 1 << 30, MaxPendingSize: 1 << 30},
	} {
		assert.NoError(t, ok.Validate(), "%+v", ok)
	}
	for _, bad := range []HubUploadsConfig{
		{MaxAttachmentSize: -1},
		{MaxWorkspaceFileSize: -1},
		{ChunkSize: -1},
		{MaxPendingSize: -1},
		{MaxAttachmentSize: MaxUploadFileSize + 1},
		{ChunkSize: MinUploadChunkSize - 1},
		{ChunkSize: MaxUploadChunkSize + 1},
		{MaxPendingSize: 1 << 20}, // below the 50 MiB default workspace limit
		{MaxWorkspaceFileSize: 1 << 30, MaxPendingSize: 1<<30 - 1},
	} {
		assert.Error(t, bad.Validate(), "%+v", bad)
	}
}

// TestHubUploadsSettings_RoundTripAndEnv: V1 to GlobalConfig and back, an
// unset block omitted, and the schema's env names reach the loaded config.
func TestHubUploadsSettings_RoundTripAndEnv(t *testing.T) {
	v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{Uploads: &V1ServerHubUploadsConfig{
		MaxAttachmentSize: 1 << 30, MaxWorkspaceFileSize: 2 << 30, ChunkSize: 4 << 20, MaxPendingSize: 8 << 30,
	}}}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, HubUploadsConfig{
		MaxAttachmentSize: 1 << 30, MaxWorkspaceFileSize: 2 << 30, ChunkSize: 4 << 20, MaxPendingSize: 8 << 30,
	}, gc.Hub.Uploads)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	require.NotNil(t, back.Hub.Uploads)
	assert.Equal(t, *v1.Hub.Uploads, *back.Hub.Uploads)
	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.Uploads, "unset limits are omitted")

	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_UPLOADS_MAXATTACHMENTSIZE", "1073741824")
	t.Setenv("SCION_SERVER_HUB_UPLOADS_MAXWORKSPACEFILESIZE", "2147483648")
	t.Setenv("SCION_SERVER_HUB_UPLOADS_CHUNKSIZE", "4194304")
	t.Setenv("SCION_SERVER_HUB_UPLOADS_MAXPENDINGSIZE", "8589934592")
	cfg, err := LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, int64(1<<30), cfg.Hub.Uploads.MaxAttachmentSize)
	assert.Equal(t, int64(2<<30), cfg.Hub.Uploads.MaxWorkspaceFileSize)
	assert.Equal(t, int64(4<<20), cfg.Hub.Uploads.ChunkSize)
	assert.Equal(t, int64(8<<30), cfg.Hub.Uploads.MaxPendingSize)
}
