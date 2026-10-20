package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzCleanRel(f *testing.F) {
	f.Add("skills/review/SKILL.md")
	f.Add("../secret")
	f.Add("C:/Windows/SKILL.md")
	f.Add(`\\server\share\SKILL.md`)
	f.Add("skills/CON/SKILL.md")
	f.Add("NUL.txt")
	f.Add("foo./SKILL.md")
	f.Fuzz(func(t *testing.T, rel string) {
		clean, err := cleanRel(rel)
		if err != nil {
			return
		}
		if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") || strings.Contains(clean, "\\") || strings.Contains(clean, "\x00") {
			t.Fatalf("cleanRel(%q) = %q", rel, clean)
		}
		if filepath.IsAbs(filepath.FromSlash(clean)) {
			t.Fatalf("absolute %q", clean)
		}
		for _, seg := range strings.Split(clean, "/") {
			if reservedDevice(seg) || forbiddenSegment(seg) || seg == "" || seg == "." || seg == ".." {
				t.Fatalf("bad segment %q in %q", seg, clean)
			}
			if seg != strings.TrimRight(seg, ". ") {
				t.Fatalf("trailing dot or space %q", seg)
			}
		}
		full := filepath.Join("/repo", filepath.FromSlash(clean))
		if !inside("/repo", full) {
			t.Fatalf("escaped %q -> %q", rel, full)
		}
	})
}

func FuzzExtractPaths(f *testing.F) {
	f.Add("see `src/main.go` and [docs](docs/a.md)")
	f.Add("https://example.com/a")
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 8000 {
			line = line[:8000]
		}
		for _, p := range extractPaths(line) {
			if strings.Contains(p, "\x00") {
				t.Fatalf("nul path %q", p)
			}
		}
	})
}

func FuzzExtractScripts(f *testing.F) {
	f.Add("run npm run build before npm run \"test:unit\"")
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > 8000 {
			line = line[:8000]
		}
		for _, s := range extractScripts(line) {
			if s == "" || strings.Contains(s, "\x00") {
				t.Fatalf("bad script %q", s)
			}
		}
	})
}

func FuzzManifest(f *testing.F) {
	f.Add([]byte(`{"holds":[{"path":"CLAUDE.md","kind":"claude"}]}`))
	f.Add([]byte(`{"holds":[{"path":"../.git/hooks/pre-commit"}]}`))
	f.Add([]byte("not json"))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<20 {
			body = body[:1<<20]
		}
		dir := t.TempDir()
		if err := writeRawManifest(dir, body); err != nil {
			t.Fatal(err)
		}
		recs, err := readManifest(dir)
		if err != nil {
			return
		}
		if len(recs) > maxInstructionFiles {
			t.Fatalf("too many records %d", len(recs))
		}
		for _, rec := range recs {
			if requireInstruction(rec.Path) != nil {
				t.Fatalf("kept non-instruction %q", rec.Path)
			}
		}
	})
}

func writeRawManifest(dir string, body []byte) error {
	if err := os.MkdirAll(filepath.Join(dir, holdRootName), 0o700); err != nil {
		return err
	}
	return os.WriteFile(manifestPath(dir), body, 0o600)
}
