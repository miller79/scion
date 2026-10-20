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

package runtimebroker

import (
	"context"
	"os/exec"
)

// execRunCommand runs name with a timeout. This platform has no process
// groups, so only the command itself is killed on timeout.
func execRunCommand(parent context.Context, name string, args ...string) ([]byte, error) {
	timeout := mountCommandTimeout
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.CombinedOutput()
	return out, classifyCommandError(name, timeout, err, ctx, parent)
}
