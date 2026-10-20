//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestFifoManifestDoesNotBlockScan(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "# Keep\n\nA fifo manifest must not stall the scan.\n")
	state := filepath.Join(dir, ".contexttax")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(state, "held.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("fifo not supported")
	}
	before := treeHash(t, dir)
	done := make(chan error, 1)
	go func() {
		_, err := Scan(dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scan blocked on a fifo manifest")
	}
	if treeHash(t, dir) != before {
		t.Fatal("scan changed the tree")
	}
}
