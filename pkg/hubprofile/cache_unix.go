//go:build unix

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

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openNoFollow makes an open refuse a symlink at the leaf.
const openNoFollow = syscall.O_NOFOLLOW

// isSymlinkErr reports whether err is the ELOOP O_NOFOLLOW returns for a
// symlink.
func isSymlinkErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

// checkPrivateFile refuses a file that others may read or that another user
// owns.
func checkPrivateFile(path string, fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("refusing %s: its mode %04o lets others read it; it must be 0600", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing %s: it is owned by another user", path)
	}
	return nil
}
