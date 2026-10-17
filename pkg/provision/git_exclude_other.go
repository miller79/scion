//go:build !unix

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

// Package provision implements Tier-1 universal workspace provisioning.
// It is a config-free leaf package that depends only on stdlib, pkg/api,
// pkg/store, pkg/util, pkg/util/fsutil (itself stdlib-only) and
// golang.org/x/sys/unix (for no-follow file operations) — deliberately
// avoiding pkg/config so that lean binaries (e.g. sciontool) can invoke
// provisioning without pulling in filesystem-based project path resolution.

package provision

import (
	"os"
	"path/filepath"
)

// openGitExclude opens hostPath/.git/info/exclude for reading and appending,
// creating info and exclude if missing. This platform has no openat or
// O_NOFOLLOW; provisioning runs on a runtime broker, which never runs here.
func openGitExclude(hostPath string) (*os.File, error) {
	info := filepath.Join(hostPath, ".git", "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(info, "exclude"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
}
