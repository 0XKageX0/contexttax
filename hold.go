package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// Hold copies an instruction file into .contexttax/held, records it, then
// removes the original. A crash after the copy keeps the original in place.
// The next scan finishes a hold whose manifest was written but whose original
// is still there.
func Hold(root, rel string) error {
	holdMu.Lock()
	defer holdMu.Unlock()
	abs, rel, err := instructionPath(root, rel)
	if err != nil {
		return err
	}
	kind, _, name, _, ok := classify(rel)
	if !ok {
		return errNotInstruction
	}
	src := filepath.Join(abs, filepath.FromSlash(rel))
	if err := rejectEscape(abs, src); err != nil {
		return err
	}
	info, err := os.Lstat(src)
	if err != nil {
		return err
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
	if err := rejectStateSymlink(abs); err != nil {
		return err
	}
	parked := parkedPath(abs, rel)
	if err := parentInside(abs, parked); err != nil {
		return err
	}
	if _, err := os.Lstat(parked); err == nil {
		// A parked file with no manifest row is an interrupted copy. Drop it.
		if err := os.Remove(parked); err != nil {
			return err
		}
	}
	body, chars, purpose, err := parkFile(src, parked, info)
	if err != nil {
		return err
	}
	preload := refinePreload(rel, kind, string(body))
	recs = append(recs, holdRecord{
		Path:    rel,
		Kind:    kind,
		Name:    name,
		Preload: preload,
		Purpose: purpose,
		When:    whenFor(rel, kind, preload, string(body)),
		Chars:   chars,
		Tokens:  chars / 4,
		HeldAt:  time.Now().UTC(),
	})
	if err := writeManifest(abs, recs); err != nil {
		_ = os.Remove(parked)
		return err
	}
	if err := os.Remove(src); err != nil {
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
	if err := rejectStateSymlink(abs); err != nil {
		return err
	}
	dest := filepath.Join(abs, filepath.FromSlash(rel))
	if !inside(abs, dest) {
		return errNotInstruction
	}
	if err := parentInside(abs, dest); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return errInUse
	}
	parked := parkedPath(abs, rel)
	if err := parentInside(abs, parked); err != nil {
		return err
	}
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
		var body []byte
		if st, err := os.Lstat(parkedPath(root, rec.Path)); err == nil && st.Mode().IsRegular() {
			if st.Size() > maxBodyBytes {
				h.Chars = int(st.Size())
				h.Tokens = h.Chars / 4
			} else if b, err := os.ReadFile(parkedPath(root, rec.Path)); err == nil {
				body = b
				if bytes.Contains(b, []byte{0}) {
					h.Chars = len(b)
				} else {
					h.Chars = utf8.RuneCount(b)
					if h.Purpose == "" {
						h.Purpose = purposeOf(string(b))
					}
				}
				h.Tokens = h.Chars / 4
			}
		}
		if h.When == "" {
			h.When = whenFor(h.Path, h.Kind, h.Preload, string(body))
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
	if rel == "" || strings.Contains(rel, "\x00") || strings.Contains(rel, ":") {
		return "", errNotInstruction
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return "", errNotInstruction
	}
	rel = strings.TrimPrefix(rel, "./")
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
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
	if err := rejectStateSymlink(root); err != nil {
		return nil, err
	}
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
	if err := rejectStateSymlink(root); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
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

// rejectEscape refuses symlinks and any path that resolves outside root.
func rejectEscape(root, full string) error {
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errNotInstruction
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return err
	}
	if !inside(root, real) {
		return errNotInstruction
	}
	return nil
}

// parentInside rejects a destination whose existing parent is a symlink
// or resolves outside root. Missing parents are checked further up.
func parentInside(root, dest string) error {
	dir := filepath.Dir(dest)
	for {
		info, err := os.Lstat(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				parent := filepath.Dir(dir)
				if parent == dir {
					return errNotInstruction
				}
				dir = parent
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errNotInstruction
		}
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if !inside(root, real) && real != root {
			return errNotInstruction
		}
		return nil
	}
}

// parkFile copies src to parked. The original is left in place.
func parkFile(src, parked string, info os.FileInfo) (body []byte, chars int, purpose string, err error) {
	if err = os.MkdirAll(filepath.Dir(parked), 0o700); err != nil {
		return nil, 0, "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return nil, 0, "", err
	}
	defer in.Close()
	out, err := os.OpenFile(parked, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, 0, "", err
	}
	failed := true
	defer func() {
		if failed {
			_ = os.Remove(parked)
		}
	}()
	if info.Size() > maxBodyBytes {
		n, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return nil, 0, "", copyErr
		}
		if closeErr != nil {
			return nil, 0, "", closeErr
		}
		failed = false
		return nil, int(n), "Larger than 4 MiB, so the body was not read. The estimate is the byte length divided by 4.", nil
	}
	b, err := io.ReadAll(io.LimitReader(in, maxBodyBytes+1))
	if err != nil {
		_ = out.Close()
		return nil, 0, "", err
	}
	if _, err = out.Write(b); err != nil {
		_ = out.Close()
		return nil, 0, "", err
	}
	if err = out.Close(); err != nil {
		return nil, 0, "", err
	}
	failed = false
	if bytes.Contains(b, []byte{0}) {
		return b, len(b), "Binary file. Line findings were skipped. The estimate is the byte length divided by 4.", nil
	}
	return b, utf8.RuneCount(b), purposeOf(string(b)), nil
}

// reconcileHolds finishes or drops holds that a crash left half-done.
// Both the original and the parked copy exist: the manifest was written and
// the original delete did not happen, so the original is removed.
// The parked copy is missing: the record is dropped and the original, if any, stays.
func reconcileHolds(root string) error {
	holdMu.Lock()
	defer holdMu.Unlock()
	recs, err := readManifest(root)
	if err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	kept := make([]holdRecord, 0, len(recs))
	changed := false
	for _, rec := range recs {
		rel, err := cleanRel(rec.Path)
		if err != nil {
			changed = true
			continue
		}
		src := filepath.Join(root, filepath.FromSlash(rel))
		parked := parkedPath(root, rel)
		if !inside(root, src) || !inside(root, parked) {
			changed = true
			continue
		}
		if err := parentInside(root, parked); err != nil {
			changed = true
			continue
		}
		srcInfo, srcErr := os.Lstat(src)
		parkInfo, parkErr := os.Lstat(parked)
		srcRegular := srcErr == nil && srcInfo.Mode().IsRegular()
		parkRegular := parkErr == nil && parkInfo.Mode().IsRegular()
		switch {
		case srcRegular && parkRegular:
			same, err := sameFileContents(src, parked)
			if err != nil {
				return err
			}
			// Identical bytes mean the delete step of Hold did not finish.
			// Different bytes mean a new file was written at the old path.
			if same {
				if err := os.Remove(src); err != nil {
					return err
				}
			}
			kept = append(kept, rec)
		case parkRegular:
			kept = append(kept, rec)
		default:
			changed = true
		}
	}
	if !changed && len(kept) == len(recs) {
		return nil
	}
	return writeManifest(root, kept)
}

func sameFileContents(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	bufA := make([]byte, 32*1024)
	bufB := make([]byte, 32*1024)
	for {
		na, errA := io.ReadFull(fa, bufA)
		nb, errB := io.ReadFull(fb, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		if errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF) {
			return errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF), nil
		}
		if errA != nil {
			return false, errA
		}
		if errB != nil && !errors.Is(errB, io.EOF) && !errors.Is(errB, io.ErrUnexpectedEOF) {
			return false, errB
		}
	}
}

// rejectStateSymlink refuses a .contexttax entry that is not a real directory
// inside root. MkdirAll and ReadFile would otherwise follow a symlink and
// write or read the hold state outside the repository.
func rejectStateSymlink(root string) error {
	dir := filepath.Join(root, holdRootName)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errNotInstruction
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if !inside(root, real) {
		return errNotInstruction
	}
	return nil
}
