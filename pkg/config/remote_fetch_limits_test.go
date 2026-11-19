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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFetchRemoteTemplateWithOptions_CountsImplicitDirectories: the
// directories a deep file path creates count toward the file limit, so an
// archive of a few files cannot create many directories.
func TestFetchRemoteTemplateWithOptions_CountsImplicitDirectories(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	deep := "root/" + strings.Repeat("d/", 30) + "f"
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: deep, Mode: 0o644, Typeflag: tar.TypeReg}))
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archive := buf.Bytes()
	download := func(context.Context, string, string) ([]byte, error) { return archive, nil }

	_, err := FetchRemoteTemplateWithOptions(context.Background(), "https://artifacts.example.com/t.tar.gz",
		RemoteFetchOptions{Download: download, MaxFiles: 10})
	require.ErrorIs(t, err, ErrRemoteFetchLimit)

	_, err = FetchRemoteTemplateWithOptions(context.Background(), "https://artifacts.example.com/t.tar.gz",
		RemoteFetchOptions{Download: download, MaxFiles: 31})
	require.NoError(t, err, "30 directories and one file fit a limit of 31")
}

// TestFetchRemoteTemplateWithOptions_NoGitFallbackWhenLimited: a limited
// fetch whose GitHub tarball fails is not retried with an unbounded git
// checkout; an unlimited fetch still is.
func TestFetchRemoteTemplateWithOptions_NoGitFallbackWhenLimited(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	restoreLs := SetGitLsRemoteForTest(func(context.Context, string, string) ([]byte, error) {
		return nil, errors.New("no git")
	})
	t.Cleanup(restoreLs)
	calls := 0
	restore := SetGitSparseCheckoutForTest(func(_ context.Context, _ *GitHubURLParts, dest, _ string) error {
		calls++
		return os.WriteFile(filepath.Join(dest, "scion-agent.yaml"), []byte("x"), 0o644)
	})
	t.Cleanup(restore)
	failing := func(context.Context, string, string) ([]byte, error) { return nil, errors.New("download refused") }
	const uri = "https://github.com/acme/repo/tree/v1.0/templates"

	_, err := FetchRemoteTemplateWithOptions(context.Background(), uri,
		RemoteFetchOptions{Download: failing, MaxFiles: 100, PrivateCacheDir: true})
	require.Error(t, err)
	assert.Equal(t, 0, calls, "a limited fetch must not run the git fallback")

	path, err := FetchRemoteTemplateWithOptions(context.Background(), uri, RemoteFetchOptions{Download: failing})
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "an unlimited fetch keeps the git fallback")
	assert.FileExists(t, filepath.Join(path, "scion-agent.yaml"))
}

// TestFetchRemoteTemplateWithOptions_RcloneCapIsPerFetch: the rclone
// transfer cap counts one fetch only, not every fetch since the process
// started.
func TestFetchRemoteTemplateWithOptions_RcloneCapIsPerFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte(strings.Repeat("x", 3000)), 0o644))
	opts := RemoteFetchOptions{MaxBytes: 4000, MaxFiles: 10, PrivateCacheDir: true}
	for i := 0; i < 3; i++ {
		_, err := FetchRemoteTemplateWithOptions(context.Background(), ":local:"+src, opts)
		require.NoError(t, err, "fetch %d", i)
	}
}

// TestFetchRemoteTemplateWithOptions_PrivateCacheDir: each fetch gets its
// own directory.
func TestFetchRemoteTemplateWithOptions_PrivateCacheDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644))
	a, err := FetchRemoteTemplateWithOptions(context.Background(), ":local:"+src, RemoteFetchOptions{PrivateCacheDir: true})
	require.NoError(t, err)
	b, err := FetchRemoteTemplateWithOptions(context.Background(), ":local:"+src, RemoteFetchOptions{PrivateCacheDir: true})
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.FileExists(t, filepath.Join(a, "f"))
	assert.FileExists(t, filepath.Join(b, "f"))
}
