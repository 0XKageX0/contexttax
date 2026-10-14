package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHoldStateSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".contexttax")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "skills", "leak", "SKILL.md"), "# Leak\n\nThis body must stay inside the repository.\n")
	err := Hold(dir, "skills/leak/SKILL.md")
	leaked := filepath.Join(outside, "held", "skills", "leak", "SKILL.md")
	if _, statErr := os.Stat(leaked); statErr == nil {
		t.Fatal("hold wrote through a symlinked .contexttax directory")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "held.json")); statErr == nil {
		t.Fatal("hold wrote a manifest through a symlinked .contexttax directory")
	}
	if err == nil {
		t.Fatal("hold followed a symlinked state directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "leak", "SKILL.md")); err != nil {
		t.Fatal("original file was removed")
	}
}

func TestReconcileDeletesOnlyIdenticalCopy(t *testing.T) {
	dir := t.TempDir()
	body := "# Keep\n\nThis instruction file is the copy that a crash left behind.\n"
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), body)
	parked := parkedPath(dir, "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(parked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parked, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(dir, []holdRecord{{
		Path: "CLAUDE.md", Kind: "claude", Name: "Claude", Preload: "always",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("identical parked copy should finish the interrupted delete")
	}

	other := t.TempDir()
	live := "# Other\n\nA different file now sits at the original path.\n"
	mustWrite(t, filepath.Join(other, "CLAUDE.md"), live)
	parked = parkedPath(other, "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(parked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parked, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(other, []holdRecord{{
		Path: "CLAUDE.md", Kind: "claude", Name: "Claude", Preload: "always",
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(other); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(other, "CLAUDE.md"))
	if err != nil || string(got) != live {
		t.Fatal("scan deleted a different file that replaced the original")
	}
}

func TestHeldFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "skills", "review", "SKILL.md"), "# Review\n\nPrivate hold contents should not be world readable.\n")
	if err := Hold(dir, "skills/review/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(dir, ".contexttax"),
		filepath.Join(dir, ".contexttax", "held"),
		filepath.Join(dir, ".contexttax", "held.json"),
		parkedPath(dir, "skills/review/SKILL.md"),
	} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o077 != 0 {
			t.Fatalf("%s mode %o is group or world accessible", path, info.Mode().Perm())
		}
	}
}

func TestOutsideSymlinkIsNotRead(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "CLAUDE.md"), []byte("OUTSIDESYMLINKSHOULDNOTAPPEAR in the report.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "CLAUDE.md"), filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf, []byte("OUTSIDESYMLINKSHOULDNOTAPPEAR")) || len(rep.Agents) != 0 {
		t.Fatalf("symlink was followed: %s", buf)
	}
}

func TestCleanRelHostile(t *testing.T) {
	for _, rel := range []string{
		"../SKILL.md",
		"/etc/passwd",
		`\\server\share\SKILL.md`,
		"C:/Windows/SKILL.md",
		"skills/CON/SKILL.md:",
		"skills/foo/../../../../SKILL.md",
		"",
		"skills/foo/SKILL.md\x00.md",
	} {
		if _, err := cleanRel(rel); err == nil {
			t.Fatalf("accepted %q", rel)
		}
	}
}

func TestPlainTextCrossOriginDoesNotHold(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "skills", "review", "SKILL.md"), "# Review skill\n\nDo not move this file from another site.\n")
	srv := httptest.NewServer(newHandler(dir))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/hold", stringsNew(`{"path":"skills/review/SKILL.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "review", "SKILL.md")); err != nil {
		t.Fatal("cross-origin post moved the file")
	}
}

func stringsNew(s string) *bytes.Reader {
	return bytes.NewReader([]byte(s))
}
