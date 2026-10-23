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

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTemplateSyncPathSource: a path argument names a template directory
// (so an agent whose project .scion is read-only can sync from a checkout);
// a bare name keeps the name lookup.
func TestTemplateSyncPathSource(t *testing.T) {
	root := t.TempDir()
	tplDir := filepath.Join(root, "repo", ".scion", "templates", "planner")
	require.NoError(t, os.MkdirAll(tplDir, 0o755))
	notDir := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o644))

	t.Run("bare name is not a path", func(t *testing.T) {
		for _, name := range []string{"planner", "custom-claude", "my_team.v2"} {
			_, isPath, err := templateSyncPathSource(name)
			assert.False(t, isPath, name)
			assert.NoError(t, err, name)
		}
	})

	t.Run("absolute directory", func(t *testing.T) {
		dir, isPath, err := templateSyncPathSource(tplDir)
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Equal(t, tplDir, dir)
		assert.Equal(t, "planner", filepath.Base(dir))
	})

	t.Run("relative forms", func(t *testing.T) {
		t.Chdir(filepath.Join(root, "repo"))
		for _, arg := range []string{"./.scion/templates/planner", ".scion/templates/planner"} {
			dir, isPath, err := templateSyncPathSource(arg)
			require.NoError(t, err, arg)
			assert.True(t, isPath, arg)
			assert.Equal(t, tplDir, dir, arg)
		}
		t.Chdir(filepath.Join(root, "repo", ".scion", "templates", "planner"))
		dir, isPath, err := templateSyncPathSource("../planner")
		require.NoError(t, err)
		assert.True(t, isPath)
		assert.Equal(t, tplDir, dir)
	})

	t.Run("path that is not a directory", func(t *testing.T) {
		for _, arg := range []string{notDir, filepath.Join(root, "missing")} {
			_, isPath, err := templateSyncPathSource(arg)
			assert.True(t, isPath, arg)
			assert.Error(t, err, arg)
		}
	})
}
