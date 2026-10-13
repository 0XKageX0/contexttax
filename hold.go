package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const holdRootName = ".contexttax"

var (
	holdMu            sync.Mutex
	errNotInstruction = errors.New("that file is not one agents preload")
	errAlreadyHeld    = errors.New("that file is already cut off")
	errNotHeld        = errors.New("that file is not cut off")
	errInUse          = errors.New("a file is already at the original path")
)

type holdRecord struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Preload string    `json:"preload"`
	Purpose string    `json:"purpose"`
	When    string    `json:"when"`
	Chars   int       `json:"chars"`
	Tokens  int       `json:"tokens"`
	HeldAt  time.Time `json:"heldAt"`
}

type holdManifest struct {
	Holds []holdRecord `json:"holds"`
}

// Hold moves an instruction file out of the path agents scan.
// The bytes stay in .contexttax/held until Release puts them back.
func Hold(root, rel string) error {
	holdMu.Lock()
	defer holdMu.Unlock()
	abs, rel, err := instructionPath(root, rel)
	if err != nil {
		return err
	}
	kind, _, name, preload, ok := classify(rel)
	if !ok {
		return errNotInstruction
	}
	src := filepath.Join(abs, filepath.FromSlash(rel))
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errNotInstruction
	}
	recs, err := readManifest(abs)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Path == rel {
			return errAlreadyHeld
		}
	}
	body, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	parked := parkedPath(abs, rel)
	if err := os.MkdirAll(filepath.Dir(parked), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(parked); err == nil {
		return errAlreadyHeld
	}
	if err := os.Rename(src, parked); err != nil {
		return err
	}
	chars := utf8.RuneCount(body)
	recs = append(recs, holdRecord{
		Path:    rel,
		Kind:    kind,
		Name:    name,
		Preload: preload,
		Purpose: purposeOf(string(body)),
		When:    whenFor(preload),
		Chars:   chars,
		Tokens:  chars / 4,
		HeldAt:  time.Now().UTC(),
	})
	if err := writeManifest(abs, recs); err != nil {
		_ = os.Rename(parked, src)
		return err
	}
	return nil
}

// Release moves a parked instruction file back where agents will load it.
func Release(root, rel string) error {
	holdMu.Lock()
	defer holdMu.Unlock()
	abs, err := absDir(root)
	if err != nil {
		return err
	}
	rel, err = cleanRel(rel)
	if err != nil {
		return err
	}
	recs, err := readManifest(abs)
	if err != nil {
		return err
	}
	kept := recs[:0]
	found := false
	for _, rec := range recs {
		if rec.Path == rel {
			found = true
			continue
		}
		kept = append(kept, rec)
	}
	if !found {
		return errNotHeld
	}
	dest := filepath.Join(abs, filepath.FromSlash(rel))
	if !inside(abs, dest) {
		return errNotInstruction
	}
	if _, err := os.Stat(dest); err == nil {
		return errInUse
	}
	parked := parkedPath(abs, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Rename(parked, dest); err != nil {
		return err
	}
	if err := writeManifest(abs, kept); err != nil {
		_ = os.Rename(dest, parked)
		return err
	}
	pruneEmpty(filepath.Dir(parked), filepath.Join(abs, holdRootName, "held"))
	tax := filepath.Join(abs, holdRootName)
	if entries, err := os.ReadDir(tax); err == nil && len(entries) == 0 {
		_ = os.Remove(tax)
	}
	return nil
}

func loadHeld(root string) []Held {
	recs, err := readManifest(root)
	if err != nil {
		return []Held{}
	}
	out := make([]Held, 0, len(recs))
	for _, rec := range recs {
		h := Held{
			Path:    rec.Path,
			Kind:    rec.Kind,
			Name:    rec.Name,
			Preload: rec.Preload,
			Purpose: rec.Purpose,
			When:    rec.When,
			Chars:   rec.Chars,
			Tokens:  rec.Tokens,
			HeldAt:  rec.HeldAt,
		}
		if b, err := os.ReadFile(parkedPath(root, rec.Path)); err == nil {
			h.Chars = utf8.RuneCount(b)
			h.Tokens = h.Chars / 4
			if h.Purpose == "" {
				h.Purpose = purposeOf(string(b))
			}
			if h.When == "" {
				h.When = whenFor(h.Preload)
			}
		}
		out = append(out, h)
	}
	return out
}

func instructionPath(root, rel string) (string, string, error) {
	abs, err := absDir(root)
	if err != nil {
		return "", "", err
	}
	rel, err = cleanRel(rel)
	if err != nil {
		return "", "", err
	}
	full := filepath.Join(abs, filepath.FromSlash(rel))
	if !inside(abs, full) {
		return "", "", errNotInstruction
	}
	if _, _, _, _, ok := classify(rel); !ok {
		return "", "", errNotInstruction
	}
	return abs, rel, nil
}

func cleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/"))
	if rel == "" || strings.Contains(rel, "\x00") || strings.HasPrefix(rel, "/") {
		return "", errNotInstruction
	}
	rel = strings.TrimPrefix(rel, "./")
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errNotInstruction
	}
	return clean, nil
}

func inside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func parkedPath(root, rel string) string {
	return filepath.Join(root, holdRootName, "held", filepath.FromSlash(rel))
}

func manifestPath(root string) string {
	return filepath.Join(root, holdRootName, "held.json")
}

func readManifest(root string) ([]holdRecord, error) {
	b, err := os.ReadFile(manifestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var man holdManifest
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, err
	}
	return man.Holds, nil
}

func writeManifest(root string, recs []holdRecord) error {
	dir := filepath.Join(root, holdRootName)
	if len(recs) == 0 {
		_ = os.Remove(manifestPath(root))
		pruneEmpty(dir, dir)
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if recs == nil {
		recs = []holdRecord{}
	}
	b, err := json.MarshalIndent(holdManifest{Holds: recs}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := manifestPath(root) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, manifestPath(root))
}

func pruneEmpty(dir, stop string) {
	for dir != stop && inside(stop, dir) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
	if dir == stop {
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) == 0 {
			_ = os.Remove(dir)
		}
	}
}
