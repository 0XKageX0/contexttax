package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFixtureRepo(t *testing.T) {
	rep, err := Scan("testdata/repo")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(rep.Root) {
		t.Fatalf("root not abs: %s", rep.Root)
	}
	if rep.TokenLimit != 2500 {
		t.Fatalf("limit %d", rep.TokenLimit)
	}
	if time.Since(rep.ScannedAt) > time.Minute || time.Until(rep.ScannedAt) > time.Second {
		t.Fatal(rep.ScannedAt)
	}

	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{
		"NODESHOULDNOTAPPEAR",
		"ROOTMDCSHOULDNOTAPPEAR",
		"IGNORETEXTSHOULDNOTAPPEAR",
	} {
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("sentinel leaked: %s", sentinel)
		}
	}

	byID := map[string]Agent{}
	var ids []string
	var fileChars int
	for _, a := range rep.Agents {
		byID[a.ID] = a
		ids = append(ids, a.ID)
		if a.Tokens != a.Chars/4 {
			t.Fatalf("%s tokens %d chars %d", a.ID, a.Tokens, a.Chars)
		}
		sum := 0
		for _, f := range a.Files {
			if strings.Contains(f.Path, `\`) {
				t.Fatalf("backslash path %s", f.Path)
			}
			b, err := os.ReadFile(filepath.Join("testdata/repo", filepath.FromSlash(f.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if f.Chars != utf8.RuneCount(b) || f.Tokens != f.Chars/4 {
				t.Fatalf("file %s chars %d want %d", f.Path, f.Chars, utf8.RuneCount(b))
			}
			sum += f.Chars
			fileChars += f.Chars
		}
		if a.Chars != sum {
			t.Fatalf("%s char sum", a.ID)
		}
	}
	wantIDs := []string{"claude", "claude:scoped", "agents", "cursor:scoped", "skill:skills/huge", "skill:skills/review"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("agents %v", ids)
	}
	if byID["claude"].Preload != "always" || byID["skill:skills/review"].Preload != "on-demand" {
		t.Fatal("preload flags")
	}
	if byID["skill:skills/review"].Name != "review" || byID["skill:skills/huge"].Name != "huge" {
		t.Fatal("skill names")
	}
	if byID["claude"].Kind != "claude" || byID["cursor:scoped"].Kind != "cursor" || byID["agents"].Name != "Agents" {
		t.Fatal("kinds")
	}
	if len(byID["claude"].Files) != 1 || byID["claude"].Files[0].Path != "CLAUDE.md" {
		t.Fatalf("claude files %+v", byID["claude"].Files)
	}
	if byID["claude:scoped"].Preload != "on-demand" || len(byID["claude:scoped"].Files) != 1 || byID["claude:scoped"].Files[0].Path != "packages/api/CLAUDE.md" {
		t.Fatalf("scoped claude %+v", byID["claude:scoped"])
	}
	if byID["cursor:scoped"].Preload != "on-demand" || len(byID["cursor:scoped"].Files) != 2 {
		t.Fatalf("cursor %+v", byID["cursor:scoped"])
	}
	if byID["skill:skills/huge"].Files[0].Tokens <= 2500 {
		t.Fatalf("huge tokens %d", byID["skill:skills/huge"].Files[0].Tokens)
	}

	if rep.Totals.Files != 7 || rep.Totals.Agents != 6 || rep.Totals.Findings != len(rep.Findings) {
		t.Fatalf("totals %+v findings %d", rep.Totals, len(rep.Findings))
	}
	if rep.Totals.Chars != fileChars || rep.Totals.Tokens != fileChars/4 {
		t.Fatalf("total chars %+v vs %d", rep.Totals, fileChars)
	}
	if rep.Totals.AlwaysChars+rep.Totals.OnDemandChars != rep.Totals.Chars {
		t.Fatal("always+ondemand")
	}
	if rep.Totals.AlwaysTokens != rep.Totals.AlwaysChars/4 || rep.Totals.OnDemandTokens != rep.Totals.OnDemandChars/4 {
		t.Fatal("split tokens")
	}
	wantAlways := byID["claude"].Chars + byID["agents"].Chars
	if rep.Totals.AlwaysChars != wantAlways {
		t.Fatalf("always chars %d want %d", rep.Totals.AlwaysChars, wantAlways)
	}

	dups := findingsOf(rep, "duplicate_line")
	if len(dups) != 1 {
		t.Fatalf("dup count %d: %+v", len(dups), texts(dups))
	}
	dup := dups[0]
	const wantText = "Always prefer the existing test harness before adding a new one."
	if dup.Text != wantText || dup.Count != 4 {
		t.Fatalf("dup %+v", dup)
	}
	if !reflect.DeepEqual(dup.Files, []string{
		".cursor/rules/style.mdc",
		"AGENTS.md",
		"CLAUDE.md",
		"packages/api/CLAUDE.md",
	}) {
		t.Fatalf("dup files %v", dup.Files)
	}
	gotLocs := map[string][]int{}
	for _, loc := range dup.Locations {
		gotLocs[loc.File] = loc.Lines
	}
	wantLocs := map[string][]int{
		".cursor/rules/style.mdc": {5},
		"AGENTS.md":               {3},
		"CLAUDE.md":               {3},
		"packages/api/CLAUDE.md":  {3},
	}
	if !reflect.DeepEqual(gotLocs, wantLocs) {
		t.Fatalf("locs %#v", gotLocs)
	}

	type pathKey struct {
		file, path string
		line       int
	}
	gotPaths := map[pathKey]int{}
	for _, f := range findingsOf(rep, "missing_path") {
		gotPaths[pathKey{f.File, f.Path, f.Line}]++
		if f.Message == "" {
			t.Fatal("empty message")
		}
	}
	wantPaths := []pathKey{
		{"AGENTS.md", "src/missing.go", 5},
		{"CLAUDE.md", "src/missing.go", 8},
		{"CLAUDE.md", "docs/nope.md", 9},
		{"skills/review/SKILL.md", "missing-notes.md", 7},
		{"skills/review/SKILL.md", "../no-such-dir/file.ts", 8},
	}
	if len(gotPaths) != len(wantPaths) {
		t.Fatalf("paths %#v", gotPaths)
	}
	for _, k := range wantPaths {
		if gotPaths[k] != 1 {
			t.Fatalf("missing %#v in %#v", k, gotPaths)
		}
	}

	type scriptKey struct {
		file, script, pkg string
		line              int
	}
	gotScripts := map[scriptKey]string{}
	for _, f := range findingsOf(rep, "missing_script") {
		gotScripts[scriptKey{f.File, f.Script, f.PackageFile, f.Line}] = f.Message
	}
	wantScripts := []scriptKey{
		{"CLAUDE.md", "lint", "package.json", 6},
		{".cursor/rules/style.mdc", "typecheck", "package.json", 7},
	}
	if len(gotScripts) != len(wantScripts) {
		t.Fatalf("scripts %#v", gotScripts)
	}
	for _, k := range wantScripts {
		msg, ok := gotScripts[k]
		if !ok || !strings.Contains(msg, "not defined in package.json") {
			t.Fatalf("script %#v msg %q", k, msg)
		}
	}

	overs := findingsOf(rep, "oversized_file")
	if len(overs) != 1 || overs[0].File != "skills/huge/SKILL.md" || overs[0].Tokens <= 2500 {
		t.Fatalf("oversized %+v", overs)
	}
	if overs[0].Tokens != overs[0].Chars/4 {
		t.Fatal("oversized formula")
	}

	rank := map[string]int{"duplicate_line": 0, "missing_path": 1, "missing_script": 2, "oversized_file": 3}
	last := -1
	for _, f := range rep.Findings {
		r, ok := rank[f.Kind]
		if !ok {
			t.Fatalf("unknown kind %s", f.Kind)
		}
		if r < last {
			t.Fatalf("out of order %s", f.Kind)
		}
		last = r
	}
}

func TestUnicodeChars(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("á", 8)
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), body)
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Agents[0].Chars != 8 || rep.Agents[0].Tokens != 2 || rep.Totals.Tokens != 2 {
		t.Fatalf("%+v totals %+v", rep.Agents[0], rep.Totals)
	}
}

func TestOversizedBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	mustWrite(t, path, strings.Repeat("x", 10000))
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Agents[0].Tokens != 2500 || len(rep.Findings) != 0 {
		t.Fatalf("exact limit tokens %d findings %+v", rep.Agents[0].Tokens, rep.Findings)
	}
	mustWrite(t, path, strings.Repeat("x", 10004))
	rep, err = Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	overs := findingsOf(rep, "oversized_file")
	if rep.Agents[0].Tokens != 2501 || len(overs) != 1 || overs[0].Tokens != 2501 {
		t.Fatalf("%+v %+v", rep.Agents[0], overs)
	}
}

func TestNearestPackageJSON(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "package.json"), `{"scripts":{"root":"echo r","shared":"echo s"}}`)
	mustWrite(t, filepath.Join(dir, "pkg", "package.json"), `{"scripts":{"shared":"echo s","local":"echo l"}}`)
	mustWrite(t, filepath.Join(dir, "pkg", "AGENTS.md"), "Run `npm run local` then `npm run root` then `npm run shared`.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]Finding{}
	for _, f := range findingsOf(rep, "missing_script") {
		missing[f.Script] = f
	}
	if _, ok := missing["root"]; !ok || missing["local"].Script != "" || missing["shared"].Script != "" {
		t.Fatalf("%+v", missing)
	}
	if missing["root"].PackageFile != "pkg/package.json" || !strings.Contains(missing["root"].Message, "not defined") {
		t.Fatalf("%+v", missing["root"])
	}
}

func TestNoPackageJSON(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), "Please run `npm run build` before shipping the change.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	scripts := findingsOf(rep, "missing_script")
	if len(scripts) != 1 || scripts[0].Script != "build" || scripts[0].PackageFile != "" {
		t.Fatalf("%+v", scripts)
	}
	if !strings.Contains(scripts[0].Message, "no package.json") {
		t.Fatal(scripts[0].Message)
	}
}

func TestBrokenPackageJSON(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "package.json"), "{")
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), "Please run `npm run build` before shipping the change.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	scripts := findingsOf(rep, "missing_script")
	if len(scripts) != 1 || !strings.Contains(scripts[0].Message, "could not be parsed") {
		t.Fatalf("%+v", scripts)
	}
}

func TestSkipsVCSAndNodeModules(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, ".git", "AGENTS.md"), "GITSHOULDNOTAPPEAR in the report body at all.\n")
	mustWrite(t, filepath.Join(dir, "node_modules", "pkg", "SKILL.md"), "NODESHOULDNOTAPPEAR in the report body at all.\n")
	mustWrite(t, filepath.Join(dir, "vendor", "lib", "SKILL.md"), "VENDORSHOULDNOTAPPEAR in the report body at all.\n")
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), "Real agent file that is long enough to be kept.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	body := string(raw)
	for _, s := range []string{"GITSHOULDNOTAPPEAR", "NODESHOULDNOTAPPEAR", "VENDORSHOULDNOTAPPEAR"} {
		if strings.Contains(body, s) {
			t.Fatalf("leaked %s", s)
		}
	}
	if len(rep.Agents) != 1 || len(rep.Agents[0].Files) != 1 {
		t.Fatalf("%+v", rep.Agents)
	}
}

func TestCursorRulesGlob(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "packages", "web", ".cursor", "rules", "voice.mdc"), "Use the shared voice guide for every user-facing string.\n")
	mustWrite(t, filepath.Join(dir, "packages", "web", ".cursor", "rules", "nested", "nope.mdc"), "This nested rule file should not be part of the scan at all.\n")
	mustWrite(t, filepath.Join(dir, "random.mdc"), "This loose mdc file should not be part of the scan at all.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Agents) != 1 || rep.Agents[0].ID != "cursor:scoped" || len(rep.Agents[0].Files) != 2 || rep.Agents[0].Preload != "on-demand" {
		t.Fatalf("%+v", rep.Agents)
	}
	got := map[string]bool{}
	for _, f := range rep.Agents[0].Files {
		got[f.Path] = true
	}
	if !got["packages/web/.cursor/rules/voice.mdc"] || !got["packages/web/.cursor/rules/nested/nope.mdc"] {
		t.Fatal(got)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("%+v", rep.Findings)
	}
}

func TestRootSkill(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "SKILL.md"), "# Root skill\n\nKeep the root skill note reasonably descriptive.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Agents) != 1 {
		t.Fatal(len(rep.Agents))
	}
	a := rep.Agents[0]
	if a.ID != "skill:." || a.Name != "skill" || a.Kind != "skill" || a.Preload != "on-demand" {
		t.Fatalf("%+v", a)
	}
}

func TestWindowsStylePath(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), "See `src\\missing.go` for the entrypoint logic.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing := findingsOf(rep, "missing_path")
	if len(missing) != 1 || missing[0].Path != "src/missing.go" {
		t.Fatalf("%+v", rep.Findings)
	}
}

func TestNotPaths(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), "Use `chars/4` or `and/or` or `read/write`, but open `src/real.go`. Import `@/lib/db` is an alias.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing := findingsOf(rep, "missing_path")
	if len(missing) != 1 || missing[0].Path != "src/real.go" {
		t.Fatalf("%+v", missing)
	}
}

func TestRelativePathExists(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "sub", "local.txt"), "hi\n")
	mustWrite(t, filepath.Join(dir, "sub", "AGENTS.md"), "Read `./local.txt` before editing `./gone.txt`.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	missing := findingsOf(rep, "missing_path")
	if len(missing) != 1 || missing[0].Path != "gone.txt" {
		t.Fatalf("%+v", missing)
	}
}

func TestScanMissingDir(t *testing.T) {
	_, err := Scan(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSkipProtectedNames(t *testing.T) {
	dir := t.TempDir()
	if !skipDirAt(dir, "node_modules") || !skipDirAt(dir, ".git") {
		t.Fatal("expected repository junk directories to be skipped")
	}
	if skipDirAt(dir, "windows") || skipDirAt(dir, "AppData") || skipDirAt(dir, "src") {
		t.Fatal("a normal repository must still scan windows, AppData, and src")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if !rootSkipsOSDirs(home) || !skipDirAt(home, "windows") || !skipDirAt(home, "$Recycle.Bin") {
		t.Fatal("profile and drive-root scans should skip OS directories")
	}
	mustWrite(t, filepath.Join(dir, "windows", "AGENTS.md"), "# Windows package\n\nThis Flutter or Electron folder is part of the repo.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Agents) != 1 || rep.Agents[0].Files[0].Path != "windows/AGENTS.md" {
		t.Fatalf("windows folder was skipped: %+v", rep.Agents)
	}
}

func TestHTTP(t *testing.T) {
	abs, err := filepath.Abs("testdata/repo")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newHandler(abs))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d type %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	var rep Report
	if err := json.NewDecoder(res.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Files != 7 || len(rep.Findings) == 0 {
		t.Fatalf("report totals %+v findings %d", rep.Totals, len(rep.Findings))
	}

	res2, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	body, _ := io.ReadAll(res2.Body)
	if res2.StatusCode != 200 || !strings.Contains(res2.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("html %d %s", res2.StatusCode, res2.Header.Get("Content-Type"))
	}
	if !bytes.Contains(body, []byte("contexttax")) || !bytes.Contains(body, []byte("api/report")) {
		t.Fatal("dashboard html missing expected copy")
	}

	res3, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer res3.Body.Close()
	if res3.StatusCode != 404 {
		t.Fatalf("nope %d", res3.StatusCode)
	}
}

func TestIndexEmbedded(t *testing.T) {
	if !bytes.Contains(indexHTML, []byte("api/report")) || !bytes.Contains(indexHTML, []byte("contexttax")) {
		t.Fatal("embedded dashboard incomplete")
	}
	if !bytes.Contains(indexHTML, []byte("Confirm cut off")) || !bytes.Contains(indexHTML, []byte("Unicode code points")) {
		t.Fatal("dashboard missing confirm step or estimate label")
	}
}

func findingsOf(rep Report, kind string) []Finding {
	var out []Finding
	for _, f := range rep.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

func texts(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Text
	}
	return out
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestTokenEstimateIsRuneCountOverFour(t *testing.T) {
	dir := t.TempDir()
	// Eight CJK characters are two estimated tokens. A model tokenizer
	// usually counts them as more than two. The scan does not.
	body := strings.Repeat("你", 8) + "\nfunc(){return}\n"
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), body)
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantChars := utf8.RuneCountInString(body)
	if rep.Agents[0].Chars != wantChars || rep.Agents[0].Tokens != wantChars/4 || rep.Totals.Tokens != wantChars/4 {
		t.Fatalf("chars %d tokens %d want chars %d", rep.Agents[0].Chars, rep.Agents[0].Tokens, wantChars)
	}
	if rep.Agents[0].Tokens == wantChars {
		t.Fatal("estimate must divide by 4")
	}
}

func TestPreloadRules(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "CLAUDE.md"), "# Project\n\nLoaded at launch from the scan root.\n")
	mustWrite(t, filepath.Join(dir, "packages", "api", "CLAUDE.md"), "# Package\n\nLoaded when Claude reads a file in this package.\n")
	mustWrite(t, filepath.Join(dir, "packages", "web", ".claude", "rules", "local.md"), "# Nested rules dir\n\nA nested .claude/rules directory is not the project rules tree.\n")
	mustWrite(t, filepath.Join(dir, "GEMINI.md"), "# Gemini root\n\nUse the project conventions for every prompt.\n")
	mustWrite(t, filepath.Join(dir, "packages", "api", "GEMINI.md"), "# Gemini nested\n\nNested Gemini context is still part of the prompt.\n")
	mustWrite(t, filepath.Join(dir, ".github", "copilot-instructions.md"), "# Copilot\n\nRepository wide instructions for every request.\n")
	mustWrite(t, filepath.Join(dir, ".github", "instructions", "go.instructions.md"), "# Go only\n\nApply these notes when editing Go files in the repo.\n")
	mustWrite(t, filepath.Join(dir, "packages", "api", "AGENTS.md"), "# Nested agents\n\nThese notes apply when work stays in this package.\n")
	mustWrite(t, filepath.Join(dir, "AGENTS.override.md"), "# Override\n\nCodex prefers this file over AGENTS.md in the same directory.\n")
	mustWrite(t, filepath.Join(dir, ".claude", "rules", "always.md"), "# Always rule\n\nThis rule has no paths field so it loads at launch.\n")
	mustWrite(t, filepath.Join(dir, ".claude", "rules", "scoped.md"), "---\npaths: [\"**/*.go\"]\n---\n\n# Scoped rule\n\nThis rule loads when a matching file is read.\n")
	mustWrite(t, filepath.Join(dir, ".cursor", "rules", "always.mdc"), "---\nalwaysApply: true\n---\n\nKeep the copyright header on every source file.\n")
	mustWrite(t, filepath.Join(dir, ".cursorrules"), "# Legacy cursor file\n\nLEGACYRULESHOULDNOTAPPEAR because the current docs do not list it.\n")
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "LEGACYRULESHOULDNOTAPPEAR") {
		t.Fatal(".cursorrules was scanned")
	}
	byID := map[string]Agent{}
	for _, a := range rep.Agents {
		byID[a.ID] = a
	}
	if byID["gemini"].Preload != "always" || len(byID["gemini"].Files) != 2 {
		t.Fatalf("gemini %+v", byID["gemini"])
	}
	geminiPaths := map[string]bool{}
	for _, f := range byID["gemini"].Files {
		geminiPaths[f.Path] = true
		if f.When != "Every session, before the task starts" {
			t.Fatalf("nested GEMINI.md must stay always-on, got %+v", f)
		}
	}
	if !geminiPaths["GEMINI.md"] || !geminiPaths["packages/api/GEMINI.md"] {
		t.Fatalf("gemini files %+v", geminiPaths)
	}
	if byID["copilot"].Preload != "always" || len(byID["copilot"].Files) != 1 {
		t.Fatalf("copilot %+v", byID["copilot"])
	}
	if byID["copilot:scoped"].Preload != "on-demand" || byID["copilot:scoped"].Files[0].Path != ".github/instructions/go.instructions.md" {
		t.Fatalf("scoped copilot %+v", byID["copilot:scoped"])
	}
	if byID["agents"].Preload != "always" || byID["agents"].Files[0].Path != "AGENTS.override.md" {
		t.Fatalf("agents %+v", byID["agents"])
	}
	if byID["agents:scoped"].Preload != "on-demand" || byID["agents:scoped"].Files[0].Path != "packages/api/AGENTS.md" {
		t.Fatalf("nested agents %+v", byID["agents:scoped"])
	}
	claudeAlways := map[string]bool{}
	for _, f := range byID["claude"].Files {
		claudeAlways[f.Path] = true
	}
	if byID["claude"].Preload != "always" || !claudeAlways["CLAUDE.md"] || !claudeAlways[".claude/rules/always.md"] {
		t.Fatalf("claude rules %+v", byID["claude"])
	}
	claudeNeed := map[string]string{}
	for _, f := range byID["claude:scoped"].Files {
		claudeNeed[f.Path] = f.When
	}
	if byID["claude:scoped"].Preload != "on-demand" ||
		claudeNeed[".claude/rules/scoped.md"] != "When Claude reads a matching file" ||
		claudeNeed["packages/api/CLAUDE.md"] != "When Claude reads a file in this directory" ||
		claudeNeed["packages/web/.claude/rules/local.md"] != "When Claude reads a file in this directory" {
		t.Fatalf("scoped rule %+v", byID["claude:scoped"])
	}
	if byID["cursor"].Preload != "always" || byID["cursor"].Files[0].Path != ".cursor/rules/always.mdc" {
		t.Fatalf("cursor %+v", byID["cursor"])
	}
}

func TestBinaryAndHugeFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "skills", "bin", "SKILL.md"), "BINARYSHOULDNOT\x00BEALINE")
	hugePath := filepath.Join(dir, "skills", "big", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(hugePath), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(hugePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("HUGEBODYSHOULDNOTBESCANNED as a line finding in this file.\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for i := 0; i < 4; i++ {
		if _, err := f.Write(buf); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(hugePath)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "BINARYSHOULDNOT") || strings.Contains(string(raw), "HUGEBODYSHOULDNOTBESCANNED") {
		t.Fatal("binary or huge body leaked into findings")
	}
	byPath := map[string]FileStat{}
	for _, a := range rep.Agents {
		for _, file := range a.Files {
			byPath[file.Path] = file
		}
	}
	bin := byPath["skills/bin/SKILL.md"]
	if bin.Chars != len("BINARYSHOULDNOT\x00BEALINE") || bin.Tokens != bin.Chars/4 || !strings.Contains(bin.Purpose, "Binary") {
		t.Fatalf("binary %+v", bin)
	}
	huge := byPath["skills/big/SKILL.md"]
	if huge.Chars != int(info.Size()) || huge.Tokens != huge.Chars/4 || !strings.Contains(huge.Purpose, "read cap") {
		t.Fatalf("huge %+v size %d", huge, info.Size())
	}
	if len(findingsOf(rep, "duplicate_line")) != 0 || len(findingsOf(rep, "missing_path")) != 0 {
		t.Fatalf("line findings %+v", rep.Findings)
	}
}

func TestUnreadableDirDoesNotFailScan(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "secret")
	mustWrite(t, filepath.Join(locked, "AGENTS.md"), "LOCKEDSHOULDNOTAPPEAR in the scan output at all.\n")
	mustWrite(t, filepath.Join(dir, "AGENTS.md"), "Visible agent instructions stay in the report.\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	probe, err := os.Open(locked)
	if err == nil {
		probe.Close()
		t.Skip("process can still read mode 000 directories")
	}
	rep, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "LOCKEDSHOULDNOTAPPEAR") || len(rep.Agents) != 1 {
		t.Fatalf("%s agents %d", raw, len(rep.Agents))
	}
}

func TestListenAddrLoopbackOnly(t *testing.T) {
	got, err := listenAddr(":7070")
	if err != nil || got != "127.0.0.1:7070" {
		t.Fatalf("%s %v", got, err)
	}
	got, err = listenAddr("0.0.0.0:8080")
	if err != nil || got != "127.0.0.1:8080" {
		t.Fatalf("%s %v", got, err)
	}
	got, err = listenAddr("localhost:9")
	if err != nil || got != "127.0.0.1:9" {
		t.Fatalf("%s %v", got, err)
	}
	got, err = listenAddr("[::1]:7070")
	if err != nil || got != "127.0.0.1:7070" {
		t.Fatalf("%s %v", got, err)
	}
	got, err = listenAddr("[::]:9")
	if err != nil || got != "127.0.0.1:9" {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := listenAddr("192.168.1.5:7070"); err == nil {
		t.Fatal("public host was accepted")
	}
}

func TestCrossOriginHoldRejected(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "skills", "review", "SKILL.md"), "# Review skill\n\nLook carefully before changing the file.\n")
	srv := httptest.NewServer(newHandler(dir))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/hold", strings.NewReader(`{"path":"skills/review/SKILL.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("evil origin %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "skills", "review", "SKILL.md")); err != nil {
		t.Fatal("file moved on a cross-origin post")
	}

	req2, err := http.NewRequest(http.MethodPost, srv.URL+"/api/hold", strings.NewReader(`{"path":"skills/review/SKILL.md"}`))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Origin", srv.URL)
	req2.Header.Set("Sec-Fetch-Site", "cross-site")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusForbidden {
		t.Fatalf("sec-fetch %d", res2.StatusCode)
	}
}
