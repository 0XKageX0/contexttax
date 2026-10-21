package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestHoldStateSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".contexttax")); err != nil {
		t.Skip("symlink not supported")
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

func TestReconcileDoesNotDeleteOnGet(t *testing.T) {
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
	before := treeHash(t, dir)
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if treeHash(t, dir) != before {
		t.Fatal("scan changed the tree")
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal("scan deleted the original")
	}
	if len(rep.Held) != 1 || !rep.Held[0].Pending {
		t.Fatalf("pending %+v", rep.Held)
	}
	if err := FinishHold(dir, "CLAUDE.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatal("explicit finish did not remove the identical original")
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
	if err := FinishHold(other, "CLAUDE.md"); !errors.Is(err, errInUse) {
		t.Fatalf("finish %v", err)
	}
	got, err := os.ReadFile(filepath.Join(other, "CLAUDE.md"))
	if err != nil || string(got) != live {
		t.Fatal("finish deleted a different file")
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
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s is a symlink", path)
		}
		// Windows reports directory bits as 0777. It does not store the Unix
		// mode passed to MkdirAll, so this check is meaningful on Unix only.
		if runtime.GOOS != "windows" && info.Mode()&0o077 != 0 {
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
		"skills/CON/SKILL.md",
		"skills/NUL.md",
		"COM1.txt",
		"LPT9.SKILL.md",
		"skills/foo./SKILL.md",
		"skills/foo /SKILL.md",
		"",
		"skills/foo/SKILL.md\x00.md",
	} {
		if _, err := cleanRel(rel); err == nil {
			t.Fatalf("accepted %q", rel)
		}
	}
	if _, err := cleanRel("..0/../../SKILL.md"); err == nil {
		t.Fatal("dotdot prefix escaped")
	}
	clean, err := cleanRel("..0")
	if err != nil {
		t.Fatal(err)
	}
	if !inside("/repo", filepath.Join("/repo", filepath.FromSlash(clean))) {
		t.Fatal("..0 left the root")
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

func treeHash(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		if d.IsDir() {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			_, _ = h.Write([]byte(info.Mode().String()))
			return nil
		}
		f, err := openReadNoFollow(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		_ = f.Close()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(h.Sum(nil))
}

func TestForgedManifestCannotCreateArbitraryFile(t *testing.T) {
	dir := t.TempDir()
	hook := filepath.Join(dir, ".github", "workflows", "evil.yml")
	if err := writeManifest(dir, []holdRecord{{
		Path: ".github/workflows/evil.yml", Kind: "skill", Name: "evil",
	}}); err != nil {
		t.Fatal(err)
	}
	recs, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("forged record kept: %+v", recs)
	}
	if err := Release(dir, ".github/workflows/evil.yml"); err == nil {
		t.Fatal("release accepted a non-instruction path")
	}
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatal("release created a workflow file")
	}
}

func TestSymlinkedManifestTempDoesNotEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "skills", "review", "SKILL.md"), "# Review\n\nStay inside the repository when the manifest is written.\n")
	state := filepath.Join(dir, ".contexttax")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(state, "held.json")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlink not supported")
	}
	before := treeHash(t, dir)
	if _, err := Scan(dir); err != nil {
		t.Fatal(err)
	}
	if treeHash(t, dir) != before {
		t.Fatal("scan followed or replaced a symlinked manifest")
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "SECRET" {
		t.Fatal("scan read or wrote the symlink target")
	}
	if err := Hold(dir, "skills/review/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(secret)
	if err != nil || string(got) != "SECRET" {
		t.Fatal("manifest write followed a symlink")
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatal("held.json is not a regular file inside the repository")
	}
}

func TestHostAndTokenAndErrorLeak(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "# Secret project\n\nDo not show this text to another host.\n")
	srv := httptest.NewServer(newHandler(dir))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/report", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || bytes.Contains(body, []byte("Secret project")) || bytes.Contains(body, []byte(dir)) {
		t.Fatalf("host status %d body %s", res.StatusCode, body)
	}

	req, err = http.NewRequest(http.MethodPost, srv.URL+"/api/hold", stringsNew(`{"path":"CLAUDE.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || bytes.Contains(body, []byte(dir)) {
		t.Fatalf("token status %d body %s", res.StatusCode, body)
	}
	if _, err := os.Stat(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatal("untokened post removed the file")
	}

	res, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("X-Frame-Options") != "DENY" || !bytes.Contains([]byte(res.Header.Get("Content-Security-Policy")), []byte("frame-ancestors 'none'")) {
		t.Fatalf("headers %v", res.Header)
	}
	page, _ := io.ReadAll(res.Body)
	if bytes.Contains(page, []byte("fonts.googleapis.com")) || bytes.Contains(page, []byte("fonts.gstatic.com")) {
		t.Fatal("dashboard still loads a remote font")
	}
}

func TestPurposeYAMLBlock(t *testing.T) {
	got := purposeOf("---\ndescription: >\n  Review the patch carefully\n---\n# Title\n")
	if got != "Review the patch carefully" {
		t.Fatalf("purpose %q", got)
	}
}

func TestWriteManifestThroughSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	parent := t.TempDir()
	link := filepath.Join(parent, "root")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink not supported")
	}
	if err := os.MkdirAll(filepath.Join(link, ".contexttax", "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(link, []holdRecord{{
		Path: "CLAUDE.md", Kind: "claude", Name: "Claude", Preload: "always",
	}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(real, ".contexttax", "held.json"))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatal("manifest was not written through the root symlink")
	}
}

func TestSymlinkedTempIsReplacedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "held.json.tmp")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlink not supported")
	}
	f, err := createNoFollow(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("inside")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "SECRET" {
		t.Fatal("temp file followed a symlink")
	}
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatal("temp path is not a regular file")
	}
}

func TestHostAllowed(t *testing.T) {
	if !hostAllowed("127.0.0.1:7070", "7070") || !hostAllowed("[::1]:7070", "7070") || !hostAllowed("localhost:7070", "7070") {
		t.Fatal("loopback host rejected")
	}
	if hostAllowed("evil.example:7070", "7070") || hostAllowed("127.0.0.1:9", "7070") || hostAllowed("example.com", "7070") {
		t.Fatal("foreign host accepted")
	}
}
