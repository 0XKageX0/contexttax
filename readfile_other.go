//go:build !unix

package main

import "os"

// Non-Unix systems have no O_NOFOLLOW in the standard library. Callers Lstat
// first and refuse a symlink. A swap between that check and the open is still
// possible; Unix builds close it with O_NOFOLLOW.
func openReadNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}

func openExclusive(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}
