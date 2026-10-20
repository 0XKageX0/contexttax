package main

import (
	"fmt"
	"io"
	"os"
)

// readLimited reads a regular file up to max bytes. A symlink, fifo, device,
// or anything larger than max is an error. The read does not follow a symlink
// at the final path. os.Root was not used: it follows symlinks and will block
// on a fifo, which is the failure this is meant to remove.
func readLimited(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if info.Size() > max {
		return nil, fmt.Errorf("file exceeds read cap")
	}
	f, err := openReadNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, st) || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if st.Size() > max {
		return nil, fmt.Errorf("file exceeds read cap")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("file exceeds read cap")
	}
	return b, nil
}
