package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
	holdMu              sync.Mutex
	errNotInstruction   = errors.New("that file is not one agents preload")
	errAlreadyHeld      = errors.New("that file is already cut off")
	errNotHeld          = errors.New("that file is not cut off")
	errInUse            = errors.New("a file is already at the original path")
	errManifestUnusable = errors.New("hold manifest is not a regular file")
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
// Opening the dashboard does not finish that delete. FinishHold does, and
// only after an explicit request.
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
	if errors.Is(err, errManifestUnusable) {
		// A symlink or fifo is hostile. Do not follow it. This explicit Hold
		// replaces that directory entry without reading the target.
		recs = nil
	} else if err != nil {
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
	if err := sameSource(src, info); err != nil {
		_ = rollbackHold(abs, rel, parked, recs[:len(recs)-1])
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
	if err := requireInstruction(rel); err != nil {
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
	parkInfo, err := os.Lstat(parked)
	if err != nil {
		return err
	}
	if parkInfo.Mode()&os.ModeSymlink != 0 || !parkInfo.Mode().IsRegular() {
		return errNotInstruction
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
		if err := requireInstruction(rec.Path); err != nil {
			continue
		}
		parked := parkedPath(root, rec.Path)
		if err := parentInside(root, parked); err != nil {
			continue
		}
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
		if st, err := os.Lstat(parked); err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
			if st.Size() > maxReadBytes {
				h.Chars = int(st.Size())
				h.Tokens = h.Chars / 4
			} else if b, err := readLimited(parked, maxReadBytes); err == nil {
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
		src := filepath.Join(root, filepath.FromSlash(rec.Path))
		if st, err := os.Lstat(src); err == nil && st.Mode().IsRegular() && st.Mode()&os.ModeSymlink == 0 {
			h.Pending = true
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
	if err := requireInstruction(rel); err != nil {
		return "", "", err
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
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, ":") || filepath.IsAbs(filepath.FromSlash(clean)) {
		return "", errNotInstruction
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errNotInstruction
		}
		if seg != strings.TrimRight(seg, ". ") {
			return "", errNotInstruction
		}
		if reservedDevice(seg) || forbiddenSegment(seg) {
			return "", errNotInstruction
		}
	}
	if len(clean) > 4096 {
		return "", errNotInstruction
	}
	return clean, nil
}

func forbiddenSegment(seg string) bool {
	switch strings.ToLower(seg) {
	case ".git", ".contexttax":
		return true
	default:
		return false
	}
}

func reservedDevice(seg string) bool {
	name := seg
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	switch strings.ToUpper(name) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	upper := strings.ToUpper(name)
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9' {
		return true
	}
	return false
}

func requireInstruction(rel string) error {
	clean, err := cleanRel(rel)
	if err != nil || clean != rel {
		return errNotInstruction
	}
	if _, _, _, _, ok := classify(rel); !ok {
		return errNotInstruction
	}
	return nil
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
	path := manifestPath(root)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errManifestUnusable
	}
	b, err := readLimited(path, 1<<20)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	var man holdManifest
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, err
	}
	if len(man.Holds) > maxInstructionFiles {
		man.Holds = man.Holds[:maxInstructionFiles]
	}
	kept := man.Holds[:0]
	for _, rec := range man.Holds {
		if requireInstruction(rec.Path) != nil {
			continue
		}
		kept = append(kept, rec)
	}
	return kept, nil
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
	if err := dropUnusableManifest(root); err != nil {
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
	tmp, f, err := createManifestTemp(dir)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return werr
		}
		if serr != nil {
			return serr
		}
		return cerr
	}
	if err := os.Rename(tmp, manifestPath(root)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func createManifestTemp(dir string) (string, *os.File, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", nil, err
	}
	name := filepath.Join(dir, "held.json."+hex.EncodeToString(rnd[:]))
	f, err := createNoFollow(name)
	if err != nil {
		return "", nil, err
	}
	return name, f, nil
}

// createNoFollow removes a symlink at path (the link, not its target) and
// creates a new regular file with O_EXCL. It never opens the symlink target.
func createNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	return openExclusive(path)
}

// dropUnusableManifest removes a symlinked or non-regular held.json.
// os.Remove on a symlink deletes the link, not the file it points at.
// Callers must use this only from an explicit hold or release, never from Scan.
func dropUnusableManifest(root string) error {
	path := manifestPath(root)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return os.Remove(path)
	}
	return nil
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
	in, err := openReadNoFollow(src)
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
	if info.Size() > maxReadBytes {
		n, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		if copyErr != nil {
			return nil, 0, "", copyErr
		}
		if closeErr != nil {
			return nil, 0, "", closeErr
		}
		failed = false
		return nil, int(n), "Larger than the read cap, so the body was not read. The estimate is the byte length divided by 4.", nil
	}
	b, err := io.ReadAll(io.LimitReader(in, maxReadBytes+1))
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

// FinishHold deletes the original instruction file after an explicit request.
// It runs only when the parked copy and the original are the same regular file
// contents. A scan never calls it.
func FinishHold(root, rel string) error {
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
	if err := requireInstruction(rel); err != nil {
		return err
	}
	recs, err := readManifest(abs)
	if err != nil {
		return err
	}
	found := false
	for _, rec := range recs {
		if rec.Path == rel {
			found = true
			break
		}
	}
	if !found {
		return errNotHeld
	}
	src := filepath.Join(abs, filepath.FromSlash(rel))
	parked := parkedPath(abs, rel)
	if err := parentInside(abs, src); err != nil || parentInside(abs, parked) != nil {
		return errNotInstruction
	}
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}
	parkInfo, err := os.Lstat(parked)
	if err != nil {
		return err
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 || parkInfo.Mode()&os.ModeSymlink != 0 || !srcInfo.Mode().IsRegular() || !parkInfo.Mode().IsRegular() {
		return errNotInstruction
	}
	same, err := sameFileContents(src, parked)
	if err != nil {
		return err
	}
	if !same {
		return errInUse
	}
	return os.Remove(src)
}

func sameSource(src string, info os.FileInfo) error {
	now, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !os.SameFile(info, now) || now.Mode()&os.ModeSymlink != 0 || !now.Mode().IsRegular() {
		return errNotInstruction
	}
	if now.Size() != info.Size() || !now.ModTime().Equal(info.ModTime()) {
		return errNotInstruction
	}
	return nil
}

func rollbackHold(root, rel, parked string, prev []holdRecord) error {
	_ = rel
	if err := os.Remove(parked); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeManifest(root, prev)
}

func sameFileContents(a, b string) (bool, error) {
	fa, err := openReadNoFollow(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := openReadNoFollow(b)
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
