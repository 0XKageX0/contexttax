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
		"NESTEDRULESHOULDNOTAPPEAR",
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
	wantIDs := []string{"claude", "agents", "cursor", "skill:skills/huge", "skill:skills/review"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("agents %v", ids)
	}
	if byID["claude"].Preload != "always" || byID["skill:skills/review"].Preload != "on-demand" {
		t.Fatal("preload flags")
	}
	if byID["skill:skills/review"].Name != "review" || byID["skill:skills/huge"].Name != "huge" {
		t.Fatal("skill names")
	}
	if byID["claude"].Kind != "claude" || byID["cursor"].Kind != "cursor" || byID["agents"].Name != "Agents" {
		t.Fatal("kinds")
	}
	if len(byID["claude"].Files) != 2 {
		t.Fatalf("claude files %+v", byID["claude"].Files)
	}
	if byID["skill:skills/huge"].Files[0].Tokens <= 2500 {
		t.Fatalf("huge tokens %d", byID["skill:skills/huge"].Files[0].Tokens)
	}

	if rep.Totals.Files != 6 || rep.Totals.Agents != 5 || rep.Totals.Findings != len(rep.Findings) {
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
	wantAlways := byID["claude"].Chars + byID["agents"].Chars + byID["cursor"].Chars
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
	if len(rep.Agents) != 1 || rep.Agents[0].ID != "cursor" || len(rep.Agents[0].Files) != 1 {
		t.Fatalf("%+v", rep.Agents)
	}
	if rep.Agents[0].Files[0].Path != "packages/web/.cursor/rules/voice.mdc" {
		t.Fatal(rep.Agents[0].Files[0].Path)
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
	if !skipDir("AppData") || !skipDir("$Recycle.Bin") || !skipDir("node_modules") {
		t.Fatal("expected protected directories to be skipped")
	}
	if skipDir("src") {
		t.Fatal("src should be scanned")
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
	if rep.Totals.Files != 6 || len(rep.Findings) == 0 {
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
