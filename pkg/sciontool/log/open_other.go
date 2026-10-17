//go:build !unix

/*
Copyright 2025 The Scion Authors.
*/


package log

import "os"

// openLogFileNoFollow opens path for appending. This platform has no
// O_NOFOLLOW; the agent log is only written inside agent containers.
func openLogFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode)
}
