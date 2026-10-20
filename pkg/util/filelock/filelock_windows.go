//go:build windows

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

package filelock

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File, flags uint32) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()), flags|windows.LOCKFILE_EXCLUSIVE_LOCK, 0, math.MaxUint32, math.MaxUint32, ol)
}

// Lock blocks until it holds an exclusive lock on f.
func Lock(f *os.File) error { return lockFile(f, 0) }

// TryLock takes an exclusive lock on f without waiting; it returns
// ErrWouldBlock when the lock is held elsewhere.
func TryLock(f *os.File) error {
	err := lockFile(f, windows.LOCKFILE_FAIL_IMMEDIATELY)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return ErrWouldBlock
	}
	return err
}

// Unlock releases a lock held on f.
func Unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, math.MaxUint32, math.MaxUint32, ol)
}
