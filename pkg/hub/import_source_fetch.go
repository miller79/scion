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

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Limits of one server-side template or harness-config source fetch
// (import, discover, harness-config reimport and skill discovery; see
// fetchRemoteForImport). Template "Refresh from Source" has its own fetcher
// and limits (defaultTemplateSourceLimits).
const (
	// importFetchTimeout bounds the whole fetch: download, extraction, the
	// git fallback and rclone copies.
	importFetchTimeout = 5 * time.Minute
	// importDownloadTimeout bounds one HTTP download, redirects included.
	importDownloadTimeout = 2 * time.Minute
	// importMaxDownloadBytes caps one HTTP download (the compressed archive
	// or GitHub tarball).
	importMaxDownloadBytes int64 = 64 << 20
	// importMaxRedirects bounds the redirect hops of one HTTP download.
	importMaxRedirects = 3
	// importMaxUnpackedBytes caps the decompressed bytes read from an
	// archive in each pass over it.
	importMaxUnpackedBytes int64 = 512 << 20
	// importMaxTreeBytes caps the file bytes written for one fetch,
	// including a GitHub tarball's temporary extraction of the whole
	// repository.
	importMaxTreeBytes int64 = 256 << 20
	// importMaxTreeFiles caps the files and directories written for one
	// fetch, counted the same way.
	importMaxTreeFiles = 20000
)

// importDownloadOverride, when set, replaces the SSRF-safe downloader for
// every server-side import fetch. It exists for the pkg/hub tests, whose
// sources are served by test servers on loopback; production code never
// sets it.
var importDownloadOverride config.RemoteDownloadFunc

// fetchRemoteForImport fetches a remote source URL to a local cache directory,
// authenticating against GitHub with the credential importCredential selects
// for the import's scope ("global", "project" or "user") and scopeID. It
// applies to both resource kinds (templates and harness-configs) and to skill
// directory discovery. The caller owns the returned cache path and must
// remove it.
//
// The source must pass checkImportSource (https://github.com, or a source
// server.hub.import_source_allowlist allows). HTTP downloads go through
// remotefetch: https on port 443 to public addresses only, re-checked on
// every redirect, so a source cannot reach the Hub's own network. The fetch
// is bounded by the import* limits above.
func (s *Server) fetchRemoteForImport(ctx context.Context, scope, scopeID, sourceURL string) (string, error) {
	fetchURI, err := s.checkImportSource(sourceURL)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, importFetchTimeout)
	defer cancel()
	path, err := config.FetchRemoteTemplateWithOptions(ctx, fetchURI, config.RemoteFetchOptions{
		Token:            s.importCredential(ctx, scope, scopeID, fetchURI),
		Download:         s.importDownloader(),
		MaxBytes:         importMaxTreeBytes,
		MaxFiles:         importMaxTreeFiles,
		MaxUnpackedBytes: importMaxUnpackedBytes,
		PrivateCacheDir:  true,
	})
	if err != nil && strings.HasPrefix(fetchURI, ":gcs:") && !errors.Is(err, config.ErrRemoteFetchLimit) {
		// Cloud Storage errors can name the Hub's service account and
		// project; keep them in the Hub log.
		if s.resourceLog != nil {
			s.resourceLog.Warn("import fetch from cloud storage failed", "source", fetchURI, "error", err)
		}
		return "", errImportGCSFetchFailed
	}
	return path, err
}

// errImportGCSFetchFailed is the caller-facing error for a failed Cloud
// Storage fetch.
var errImportGCSFetchFailed = errors.New("the Cloud Storage source could not be read; check the path and that the Hub has access to it")

// checkImportSource returns the URI to fetch for sourceURL, or an error
// (wrapping config.ErrImportSourceNotAllowed, without the URL) when the Hub
// may not fetch it.
func (s *Server) checkImportSource(sourceURL string) (string, error) {
	return config.CheckHubImportSource(sourceURL, s.config.ImportSourceAllowlist)
}

// importDownloader returns the download hook for server-side import fetches.
func (s *Server) importDownloader() config.RemoteDownloadFunc {
	if importDownloadOverride != nil {
		return importDownloadOverride
	}
	return newImportDownloader(s.resourceLog)
}

// newImportDownloader returns the SSRF-safe download hook. An unauthenticated
// download may reach any public https host (an allowlisted archive host, or
// GitHub's download host after a redirect); a download carrying a GitHub
// credential is limited to the GitHub hosts on every hop, and the
// credential goes to the first host only.
func newImportDownloader(logger *slog.Logger) config.RemoteDownloadFunc {
	if logger == nil {
		logger = slog.Default()
	}
	open := remotefetch.New(remotefetch.Config{
		MaxBytes:     importMaxDownloadBytes,
		Timeout:      importDownloadTimeout,
		MaxRedirects: importMaxRedirects,
		Logger:       logger,
	})
	github := remotefetch.New(remotefetch.Config{
		MaxBytes:     importMaxDownloadBytes,
		Timeout:      importDownloadTimeout,
		MaxRedirects: importMaxRedirects,
		AllowedHosts: templateSourceHosts,
		Logger:       logger,
	})
	return func(ctx context.Context, rawURL, authorization string) ([]byte, error) {
		var res *remotefetch.Result
		var err error
		if authorization != "" {
			res, err = github.FetchBytesAuthorized(ctx, rawURL, authorization)
		} else {
			res, err = open.FetchBytes(ctx, rawURL)
		}
		if err != nil {
			return nil, importDownloadError(err)
		}
		return res.Body, nil
	}
}

// errImportDownloadFailed is the caller-facing error for a failed import
// download. remotefetch logs the reason and detail server-side; the caller
// never learns whether an address was refused, a status code, or a body,
// so a source cannot be used to probe the Hub's network.
var errImportDownloadFailed = errors.New("the source could not be downloaded; check the URL and that the source is public or a repository credential is set")

// importDownloadError maps a remotefetch error to a fixed message.
func importDownloadError(err error) error {
	var fe *remotefetch.Error
	if errors.As(err, &fe) {
		switch fe.Reason {
		case remotefetch.ReasonTooLarge:
			return fmt.Errorf("%w (the download is larger than %d MiB)", config.ErrRemoteFetchLimit, importMaxDownloadBytes>>20)
		case remotefetch.ReasonTimeout:
			return config.ErrRemoteDownloadTimeout
		}
	}
	return errImportDownloadFailed
}
