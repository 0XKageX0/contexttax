package main

import (
	"bytes"
	"encoding/json"
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
