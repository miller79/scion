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

import "fmt"

// Upload limit defaults and bounds (settings server.hub.uploads).
const (
	// DefaultUploadMaxAttachmentSize is the per-file limit of a chat
	// attachment (stored outside the artifact service) when
	// server.hub.uploads.max_attachment_size is unset.
	DefaultUploadMaxAttachmentSize int64 = 10 << 20
	// DefaultUploadMaxWorkspaceFileSize is the per-file limit of a project
	// workspace or shared directory upload when
	// server.hub.uploads.max_workspace_file_size is unset.
	DefaultUploadMaxWorkspaceFileSize int64 = 50 << 20
	// DefaultUploadChunkSize is the size of one chunk of a chunked upload
	// when server.hub.uploads.chunk_size is unset. It is kept well under the
	// request body limits common on load balancers and reverse proxies.
	DefaultUploadChunkSize int64 = 8 << 20
	// MinUploadChunkSize and MaxUploadChunkSize bound chunk_size.
	MinUploadChunkSize int64 = 256 << 10
	MaxUploadChunkSize int64 = 64 << 20
	// MaxUploadFileSize is the largest per-file limit accepted.
	MaxUploadFileSize int64 = 64 << 30
	// minDefaultUploadPendingSize is the floor of the default
	// max_pending_size.
	minDefaultUploadPendingSize int64 = 1 << 30
)

// HubUploadsConfig holds web upload limits (settings server.hub.uploads).
// Read at startup. Zero means the default for every field.
type HubUploadsConfig struct {
	// MaxAttachmentSize is the per-file limit, in bytes, of a chat
	// attachment stored by the hub's attachment store. Attachments stored in
	// the artifact service keep that service's own file size limit.
	MaxAttachmentSize int64 `json:"maxAttachmentSize,omitempty" yaml:"maxAttachmentSize,omitempty" koanf:"maxAttachmentSize"`
	// MaxWorkspaceFileSize is the per-file limit, in bytes, of an upload to
	// a project workspace or shared directory.
	MaxWorkspaceFileSize int64 `json:"maxWorkspaceFileSize,omitempty" yaml:"maxWorkspaceFileSize,omitempty" koanf:"maxWorkspaceFileSize"`
	// ChunkSize is the size, in bytes, of one chunk of a chunked upload: the
	// largest request body a chunked upload sends.
	ChunkSize int64 `json:"chunkSize,omitempty" yaml:"chunkSize,omitempty" koanf:"chunkSize"`
	// MaxPendingSize caps, in bytes, the declared sizes of all chunked
	// uploads in progress on this hub instance (their temporary files).
	MaxPendingSize int64 `json:"maxPendingSize,omitempty" yaml:"maxPendingSize,omitempty" koanf:"maxPendingSize"`
}

// IsZero reports whether no upload limit is configured.
func (c HubUploadsConfig) IsZero() bool { return c == HubUploadsConfig{} }

// EffectiveMaxAttachmentSize returns MaxAttachmentSize, or its default.
func (c HubUploadsConfig) EffectiveMaxAttachmentSize() int64 {
	if c.MaxAttachmentSize > 0 {
		return c.MaxAttachmentSize
	}
	return DefaultUploadMaxAttachmentSize
}

// EffectiveMaxWorkspaceFileSize returns MaxWorkspaceFileSize, or its default.
func (c HubUploadsConfig) EffectiveMaxWorkspaceFileSize() int64 {
	if c.MaxWorkspaceFileSize > 0 {
		return c.MaxWorkspaceFileSize
	}
	return DefaultUploadMaxWorkspaceFileSize
}

// EffectiveChunkSize returns ChunkSize, or its default.
func (c HubUploadsConfig) EffectiveChunkSize() int64 {
	if c.ChunkSize > 0 {
		return c.ChunkSize
	}
	return DefaultUploadChunkSize
}

// EffectiveMaxPendingSize returns MaxPendingSize, or its default: four
// times the larger per-file limit, at least 1 GiB.
func (c HubUploadsConfig) EffectiveMaxPendingSize() int64 {
	if c.MaxPendingSize > 0 {
		return c.MaxPendingSize
	}
	n := 4 * max(c.EffectiveMaxAttachmentSize(), c.EffectiveMaxWorkspaceFileSize())
	return max(n, minDefaultUploadPendingSize)
}

// Validate reports a value the hub refuses at startup: a negative value, a
// per-file limit above MaxUploadFileSize, a chunk size outside
// [MinUploadChunkSize, MaxUploadChunkSize], or a max_pending_size smaller
// than a per-file limit (no upload of that size could ever start).
func (c HubUploadsConfig) Validate() error {
	for _, f := range []struct {
		key string
		v   int64
	}{
		{"max_attachment_size", c.MaxAttachmentSize},
		{"max_workspace_file_size", c.MaxWorkspaceFileSize},
		{"chunk_size", c.ChunkSize},
		{"max_pending_size", c.MaxPendingSize},
	} {
		if f.v < 0 {
			return fmt.Errorf("invalid server.hub.uploads.%s %d: must not be negative", f.key, f.v)
		}
	}
	for _, f := range []struct {
		key string
		v   int64
	}{
		{"max_attachment_size", c.MaxAttachmentSize},
		{"max_workspace_file_size", c.MaxWorkspaceFileSize},
	} {
		if f.v > MaxUploadFileSize {
			return fmt.Errorf("invalid server.hub.uploads.%s %d: must be at most %d", f.key, f.v, MaxUploadFileSize)
		}
	}
	if c.ChunkSize != 0 && (c.ChunkSize < MinUploadChunkSize || c.ChunkSize > MaxUploadChunkSize) {
		return fmt.Errorf("invalid server.hub.uploads.chunk_size %d: must be between %d and %d",
			c.ChunkSize, MinUploadChunkSize, MaxUploadChunkSize)
	}
	if c.MaxPendingSize != 0 {
		if largest := max(c.EffectiveMaxAttachmentSize(), c.EffectiveMaxWorkspaceFileSize()); c.MaxPendingSize < largest {
			return fmt.Errorf("invalid server.hub.uploads.max_pending_size %d: must be at least the largest per-file limit (%d)",
				c.MaxPendingSize, largest)
		}
	}
	return nil
}
