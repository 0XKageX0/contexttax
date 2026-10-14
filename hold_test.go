package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHoldAndRelease(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "skills", "huge")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\ndescription: spends the whole window\n---\n\n" + strings.Repeat("token ", 80)
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not an instruction"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A parked copy must not be billed as a second skill.
	if err := os.MkdirAll(filepath.Join(dir, ".contexttax", "held", "decoy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".contexttax", "held", "decoy", "SKILL.md"), []byte("DECOYSHOULDNOTAPPEAR"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Hold(dir, "README.md"); err == nil {
		t.Fatal("held a file agents do not preload")
	}
	if err := Hold(dir, "../SKILL.md"); err == nil {
		t.Fatal("held a path outside the repo")
	}
	if err := Hold(dir, "skills/huge/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(skill, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("original skill still in place")
	}

	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "DECOYSHOULDNOTAPPEAR") {
		t.Fatal("parked decoy was billed")
	}
	if len(rep.Agents) != 0 || rep.Totals.Tokens != 0 {
		t.Fatalf("live bill agents=%d tokens=%d", len(rep.Agents), rep.Totals.Tokens)
	}
	if len(rep.Held) != 1 || rep.Held[0].Path != "skills/huge/SKILL.md" {
		t.Fatalf("held %+v", rep.Held)
	}
	if rep.Held[0].Purpose != "spends the whole window" || rep.Held[0].Tokens != rep.Held[0].Chars/4 || rep.Totals.HeldTokens == 0 {
		t.Fatalf("held detail %+v totals %+v", rep.Held[0], rep.Totals)
	}

	if err := os.RemoveAll(filepath.Join(dir, ".contexttax", "held", "decoy")); err != nil {
		t.Fatal(err)
	}
	if err := Release(dir, "skills/huge/SKILL.md"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(skill, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatal("restored bytes differ")
	}
	if _, err := os.Stat(filepath.Join(dir, ".contexttax")); !os.IsNotExist(err) {
		t.Fatal(".contexttax left behind after the only hold was released")
	}
	rep, err = Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Held) != 0 || len(rep.Agents) != 1 || rep.Agents[0].Files[0].Purpose != "spends the whole window" {
		t.Fatalf("after release held=%d agents=%+v", len(rep.Held), rep.Agents)
	}
}

func TestHoldAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "review", "SKILL.md"), []byte("# Review skill\n\nLook carefully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newHandler(dir))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/hold", "application/json", bytes.NewBufferString(`{"path":"skills/review/SKILL.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("hold status %d", res.StatusCode)
	}
	var rep Report
	if err := json.NewDecoder(res.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Held) != 1 || len(rep.Agents) != 0 {
		t.Fatalf("hold response agents=%d held=%d", len(rep.Agents), len(rep.Held))
	}

	res2, err := http.Post(srv.URL+"/api/hold", "application/json", bytes.NewBufferString(`{"path":"nope.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad path status %d", res2.StatusCode)
	}
}

func TestHoldRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "SKILL.md")
	body := []byte("# Outside\n\nThis file must stay outside the scanned root.\n")
	if err := os.WriteFile(secret, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "skills", "SKILL.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlink not supported")
	}
	if err := Hold(dir, "skills/SKILL.md"); err == nil {
		t.Fatal("held a symlink")
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != string(body) {
		t.Fatal("outside file changed")
	}

	nested := filepath.Join(outside, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	outsideSkill := filepath.Join(nested, "SKILL.md")
	if err := os.WriteFile(outsideSkill, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := Hold(dir, "linked/SKILL.md"); err == nil {
		t.Fatal("held through a symlinked directory")
	}
	if _, err := os.Stat(outsideSkill); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"/etc/passwd", "C:/Windows/SKILL.md", `..\..\SKILL.md`} {
		if err := Hold(dir, rel); err == nil {
			t.Fatalf("accepted %s", rel)
		}
	}
}

func TestHoldSameBaseName(t *testing.T) {
	dir := t.TempDir()
	a := "skills/a/SKILL.md"
	b := "skills/b/SKILL.md"
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(a)), "# Skill A\n\nFirst skill body that is long enough.\n")
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(b)), "# Skill B\n\nSecond skill body that is long enough.\n")
	if err := Hold(dir, a); err != nil {
		t.Fatal(err)
	}
	if err := Hold(dir, b); err != nil {
		t.Fatal(err)
	}
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Held) != 2 || len(rep.Agents) != 0 {
		t.Fatalf("held %+v agents %d", rep.Held, len(rep.Agents))
	}
	if err := Release(dir, a); err != nil {
		t.Fatal(err)
	}
	rep, err = Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Agents) != 1 || rep.Agents[0].Files[0].Path != a || len(rep.Held) != 1 || rep.Held[0].Path != b {
		t.Fatalf("after one release agents %+v held %+v", rep.Agents, rep.Held)
	}
}

func TestReleaseWhenOriginalExists(t *testing.T) {
	dir := t.TempDir()
	rel := "skills/review/SKILL.md"
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)), "# Review\n\nOriginal skill body for the hold test.\n")
	if err := Hold(dir, rel); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, filepath.FromSlash(rel)), "# Replacement\n\nA new file now occupies the original path.\n")
	err := Release(dir, rel)
	if !errors.Is(err, errInUse) {
		t.Fatalf("err %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil || !strings.Contains(string(got), "Replacement") {
		t.Fatal("replacement was overwritten")
	}
	parked, err := os.ReadFile(parkedPath(dir, rel))
	if err != nil || !strings.Contains(string(parked), "Original") {
		t.Fatal("parked copy missing")
	}
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Held) != 1 || len(rep.Agents) != 1 {
		t.Fatalf("held %d agents %d", len(rep.Held), len(rep.Agents))
	}
}

func TestReconcileInterruptedHold(t *testing.T) {
	dir := t.TempDir()
	rel := "skills/review/SKILL.md"
	body := "# Review\n\nOriginal skill body that a crash must not lose.\n"
	src := filepath.Join(dir, filepath.FromSlash(rel))
	mustWrite(t, src, body)
	parked := parkedPath(dir, rel)
	if err := os.MkdirAll(filepath.Dir(parked), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parked, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(dir, []holdRecord{{
		Path: rel, Kind: "skill", Name: "review", Preload: "on-demand",
		Purpose: "Review", When: "Only when this skill is loaded",
		Chars: len(body), Tokens: len(body) / 4,
	}}); err != nil {
		t.Fatal(err)
	}
	// Manifest written, original still present: the crash window.
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("interrupted hold left the original in place")
	}
	if len(rep.Held) != 1 || len(rep.Agents) != 0 {
		t.Fatalf("held %+v agents %d", rep.Held, len(rep.Agents))
	}
	got, err := os.ReadFile(parked)
	if err != nil || string(got) != body {
		t.Fatal("parked bytes lost")
	}
}
