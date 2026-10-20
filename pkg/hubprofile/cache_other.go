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

package hubprofile

import "io/fs"

// openNoFollow is zero: this platform has no O_NOFOLLOW. Creating a
// symlink there needs a privilege ordinary users do not hold.
const openNoFollow = 0

// isSymlinkErr reports false: without O_NOFOLLOW no open refuses a symlink.
func isSymlinkErr(error) bool { return false }

// checkPrivateFile accepts the file: this platform has no unix permission
// bits or numeric owner (Go reports a fixed mode), so the cache's privacy
// rests on its location under the user's own profile directory and its ACLs.
func checkPrivateFile(string, fs.FileInfo) error { return nil }
