package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// oversizedTokenLimit is exclusive: a file is flagged only when the estimate is greater.
	oversizedTokenLimit = 2500
	// minDuplicateRunes ignores fences, rulers, and other short lines.
	minDuplicateRunes = 20
)

// Scan caps. Tests and flags may lower them. maxReadBytes is how much of one
// instruction file is read; a larger file is still billed from its lstat size.
var (
	maxReadBytes        int64 = 256 << 10
	maxInstructionFiles       = 2000
	maxWalkEntries            = 50000
	// estimateBytes, when set from -estimate bytes, counts bytes instead of
	// Unicode code points. The divisor is still 4.
	estimateBytes = false
)

var alwaysSkipDir = map[string]struct{}{
	"node_modules": {}, ".git": {}, "vendor": {}, "dist": {},
	".next": {}, "coverage": {}, ".turbo": {}, ".cache": {},
	".tools": {}, ".contexttax": {},
}

// profileSkipDir is applied only when the scan root is a drive root or the
// user profile. A repository that contains a windows/ folder is not one of those.
var profileSkipDir = map[string]struct{}{
	"appdata": {}, "application data": {}, "local settings": {},
	"$recycle.bin": {}, "system volume information": {},
	"windows": {}, "program files": {}, "program files (x86)": {},
	"programdata": {}, "recovery": {},
}

var knownExt = map[string]struct{}{
	".md": {}, ".mdc": {}, ".json": {}, ".ts": {}, ".tsx": {}, ".js": {}, ".jsx": {},
	".mjs": {}, ".cjs": {}, ".go": {}, ".py": {}, ".rs": {}, ".rb": {}, ".yml": {},
	".yaml": {}, ".toml": {}, ".css": {}, ".scss": {}, ".html": {}, ".htm": {},
	".sh": {}, ".txt": {}, ".lock": {}, ".svg": {}, ".sql": {}, ".xml": {},
	".mod": {}, ".sum": {},
}

// pathRoots are extensionless first segments that still denote a file path
// (src/lib) rather than prose (and/or, chars/4).
var pathRoots = map[string]struct{}{
	"src": {}, "app": {}, "lib": {}, "docs": {}, "doc": {}, "public": {},
	"scripts": {}, "script": {}, "server": {}, "cmd": {}, "internal": {},
	"pkg": {}, "bin": {}, "test": {}, "tests": {}, "testdata": {},
	"web": {}, "packages": {}, "components": {}, "pages": {}, "routes": {},
	"api": {}, "migrations": {}, "assets": {}, "dist": {}, "build": {},
	"config": {}, "configs": {}, "skills": {}, "rules": {}, "content": {},
	"static": {}, "templates": {}, "template": {}, "db": {}, "prisma": {},
}

var (
	codeSpanRe   = regexp.MustCompile("`([^`]+)`")
	mdLinkRe     = regexp.MustCompile(`\[[^\]\n]{0,300}\]\(([^)\n\s]+)`)
	npmRunRe     = regexp.MustCompile(`(?i)\bnpm\s+run\s+(?:"([^"]+)"|'([^']+)'|([^\s"'` + "`" + `&;|<>=]+))`)
	scriptNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:@/+=-]*$`)
	pathSegRe    = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)
)

// Report is the JSON body of GET /api/report.
type Report struct {
	Root       string    `json:"root"`
	ScannedAt  time.Time `json:"scannedAt"`
	TokenLimit int       `json:"tokenLimit"`
	Agents     []Agent   `json:"agents"`
	Totals     Totals    `json:"totals"`
	Findings   []Finding `json:"findings"`
	Held       []Held    `json:"held"`
	Truncated  bool      `json:"truncated,omitempty"`
	Estimate   string    `json:"estimate"`
}

// Agent is one preload audience: Claude, Agents, Cursor, or a single skill.
type Agent struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Preload string     `json:"preload"`
	Chars   int        `json:"chars"`
	Tokens  int        `json:"tokens"`
	Files   []FileStat `json:"files"`
}

// FileStat is one scanned instruction file.
type FileStat struct {
	Path    string `json:"path"`
	Chars   int    `json:"chars"`
	Tokens  int    `json:"tokens"`
	Purpose string `json:"purpose"`
	When    string `json:"when"`
}

// Held is an instruction file parked out of the paths agents load.
// It stays out until the user allows it back.
type Held struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Preload string    `json:"preload"`
	Purpose string    `json:"purpose"`
	When    string    `json:"when"`
	Chars   int       `json:"chars"`
	Tokens  int       `json:"tokens"`
	HeldAt  time.Time `json:"heldAt"`
	Pending bool      `json:"pending,omitempty"`
}

// Totals roll the whole scan up. Tokens are floor(chars/4) on the combined
// text, not the sum of per-file floors.
type Totals struct {
	Agents         int `json:"agents"`
	Files          int `json:"files"`
	Chars          int `json:"chars"`
	Tokens         int `json:"tokens"`
	AlwaysChars    int `json:"alwaysChars"`
	AlwaysTokens   int `json:"alwaysTokens"`
	OnDemandChars  int `json:"onDemandChars"`
	OnDemandTokens int `json:"onDemandTokens"`
	Findings       int `json:"findings"`
	HeldFiles      int `json:"heldFiles"`
	HeldChars      int `json:"heldChars"`
	HeldTokens     int `json:"heldTokens"`
}

// Finding is one duplicated line, missing path, missing npm script, or oversized file.
type Finding struct {
	Kind        string     `json:"kind"`
	Message     string     `json:"message"`
	File        string     `json:"file,omitempty"`
	Line        int        `json:"line,omitempty"`
	Text        string     `json:"text,omitempty"`
	Path        string     `json:"path,omitempty"`
	Script      string     `json:"script,omitempty"`
	PackageFile string     `json:"packageFile,omitempty"`
	Tokens      int        `json:"tokens,omitempty"`
	Chars       int        `json:"chars,omitempty"`
	Count       int        `json:"count,omitempty"`
	Files       []string   `json:"files,omitempty"`
	Locations   []Location `json:"locations,omitempty"`
}

// Location is where a duplicated line appears in one file.
type Location struct {
	File  string `json:"file"`
	Lines []int  `json:"lines"`
}

type sourceFile struct {
	Rel       string
	Abs       string
	Kind      string
	AgentID   string
	AgentName string
	Preload   string
	Purpose   string
	When      string
	Body      string
	Lines     []string
	Chars     int
	Tokens    int
}

type scriptSet struct {
	found   bool
	broken  bool
	rel     string
	scripts map[string]bool
}

type scanner struct {
	root  string
	cache map[string]scriptSet
}

// Scan walks root and returns the preload report.
func Scan(root string) (Report, error) {
	abs, err := absDir(root)
	if err != nil {
		return Report{}, err
	}
	sources, truncated, err := collectSources(abs)
	if err != nil {
		return Report{}, err
	}
	rep := Report{
		Root:       abs,
		ScannedAt:  time.Now().UTC(),
		TokenLimit: oversizedTokenLimit,
		Agents:     []Agent{},
		Findings:   []Finding{},
		Held:       []Held{},
		Truncated:  truncated,
		Estimate:   estimateName(),
	}
	rep.Agents = buildAgents(sources)
	s := &scanner{root: abs, cache: map[string]scriptSet{}}
	rep.Findings = append(rep.Findings, duplicateFindings(sources)...)
	rep.Findings = append(rep.Findings, s.pathFindings(sources)...)
	rep.Findings = append(rep.Findings, s.scriptFindings(sources)...)
	rep.Findings = append(rep.Findings, oversizedFindings(sources)...)
	sortFindings(rep.Findings)

	for _, a := range rep.Agents {
		rep.Totals.Files += len(a.Files)
		rep.Totals.Chars += a.Chars
		if a.Preload == "on-demand" {
			rep.Totals.OnDemandChars += a.Chars
		} else {
			rep.Totals.AlwaysChars += a.Chars
		}
	}
	rep.Totals.Agents = len(rep.Agents)
	rep.Totals.Tokens = rep.Totals.Chars / 4
	rep.Totals.AlwaysTokens = rep.Totals.AlwaysChars / 4
	rep.Totals.OnDemandTokens = rep.Totals.OnDemandChars / 4
	rep.Totals.Findings = len(rep.Findings)
	rep.Held = loadHeld(abs)
	for _, h := range rep.Held {
		rep.Totals.HeldFiles++
		rep.Totals.HeldChars += h.Chars
	}
	rep.Totals.HeldTokens = rep.Totals.HeldChars / 4
	return rep, nil
}

func absDir(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	abs = resolved
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

func estimateName() string {
	if estimateBytes {
		return "bytes/4"
	}
	return "runes/4"
}

func collectSources(root string) ([]sourceFile, bool, error) {
	var out []sourceFile
	truncated := false
	walked := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		walked++
		if walked > maxWalkEntries {
			truncated = true
			return fs.SkipAll
		}
		if err != nil {
			if d == nil || d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirAt(root, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if len(out) >= maxInstructionFiles {
			truncated = true
			return fs.SkipAll
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		kind, id, name, _, ok := classify(rel)
		if !ok {
			return nil
		}
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		var (
			body    string
			lines   []string
			chars   int
			purpose string
		)
		switch {
		case info.Size() > maxReadBytes:
			chars = int(info.Size())
			purpose = "Larger than the read cap, so the body was not read. The estimate is the byte length divided by 4."
		default:
			b, err := readLimited(p, maxReadBytes)
			if err != nil {
				return nil
			}
			if bytes.Contains(b, []byte{0}) {
				chars = len(b)
				purpose = "Binary file. Line findings were skipped. The estimate is the byte length divided by 4."
			} else {
				body = strings.ToValidUTF8(string(b), "")
				if estimateBytes {
					chars = len(b)
				} else {
					chars = utf8.RuneCountInString(body)
				}
				lines = strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
				purpose = purposeOf(body)
			}
		}
		preload := refinePreload(rel, kind, body)
		if kind != "skill" && preload == "on-demand" {
			id += ":scoped"
		}
		out = append(out, sourceFile{
			Rel:       rel,
			Abs:       p,
			Kind:      kind,
			AgentID:   id,
			AgentName: name,
			Preload:   preload,
			Purpose:   purpose,
			When:      whenFor(rel, kind, preload, body),
			Body:      body,
			Lines:     lines,
			Chars:     chars,
			Tokens:    chars / 4,
		})
		return nil
	})
	if err != nil {
		return nil, truncated, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, truncated, nil
}

func skipDirAt(root, name string) bool {
	low := strings.ToLower(name)
	if _, ok := alwaysSkipDir[low]; ok {
		return true
	}
	if _, ok := profileSkipDir[low]; ok && rootSkipsOSDirs(root) {
		return true
	}
	return false
}

// rootSkipsOSDirs is true for a filesystem root or the user profile.
// A project directory that merely contains windows/ is not skipped.
func rootSkipsOSDirs(root string) bool {
	clean := filepath.Clean(root)
	if clean == string(filepath.Separator) {
		return true
	}
	vol := filepath.VolumeName(clean)
	if vol != "" && strings.EqualFold(clean, vol+string(filepath.Separator)) {
		return true
	}
	home, err := os.UserHomeDir()
	if err == nil && samePath(clean, home) {
		return true
	}
	return false
}

func classify(rel string) (kind, id, name, preload string, ok bool) {
	rel = filepath.ToSlash(rel)
	base := path.Base(rel)
	switch base {
	case "CLAUDE.md", "CLAUDE.local.md":
		return "claude", "claude", "Claude", "always", true
	case "AGENTS.md", "AGENTS.override.md":
		return "agents", "agents", "Agents", "always", true
	case "GEMINI.md":
		return "gemini", "gemini", "Gemini", "always", true
	case "SKILL.md":
		dir := path.Dir(rel)
		skill := path.Base(dir)
		if dir == "." || skill == "." || skill == "" || skill == "/" {
			return "skill", "skill:.", "skill", "on-demand", true
		}
		return "skill", "skill:" + dir, skill, "on-demand", true
	}
	if rel == ".github/copilot-instructions.md" {
		return "copilot", "copilot", "Copilot", "always", true
	}
	if strings.HasPrefix(rel, ".github/instructions/") && strings.HasSuffix(base, ".instructions.md") {
		return "copilot", "copilot", "Copilot", "on-demand", true
	}
	if strings.Contains("/"+rel, "/.claude/rules/") && strings.HasSuffix(base, ".md") {
		return "claude", "claude", "Claude", "always", true
	}
	if strings.Contains("/"+rel, "/.cursor/rules/") && strings.HasSuffix(base, ".mdc") {
		return "cursor", "cursor", "Cursor", "on-demand", true
	}
	return "", "", "", "", false
}

// refinePreload applies launch-versus-on-demand rules from the agent docs.
// Claude Code loads CLAUDE.md from the working directory upward at launch, and a
// subdirectory CLAUDE.md only when it reads or edits a file there
// (https://code.claude.com/docs/en/memory). Rules under .claude/rules/ with no
// paths field load at launch; a paths field loads on demand. Codex walks
// AGENTS.md from the repo root down to the working directory and stops there
// (https://developers.openai.com/codex/guides/agents-md). Cursor applies a nested
// AGENTS.md when work is in that directory, and a .mdc rule is always-on only
// when alwaysApply is true (https://cursor.com/docs/rules). Gemini CLI
// concatenates every GEMINI.md under the working directory into each prompt
// (https://google-gemini.github.io/gemini-cli/docs/cli/gemini-md.html).
// .github/copilot-instructions.md applies to every Copilot request in the repo
// (https://docs.github.com/en/copilot/customizing-copilot/adding-custom-instructions-for-github-copilot).
func refinePreload(rel, kind, body string) string {
	rel = filepath.ToSlash(rel)
	dir := path.Dir(rel)
	switch kind {
	case "claude":
		if strings.Contains("/"+rel, "/.claude/rules/") {
			if frontmatterHas(body, "paths") {
				return "on-demand"
			}
			// Recursive files inside the project .claude/rules tree load at launch.
			// A .claude/rules directory nested in another package is not that tree.
			if strings.HasPrefix(rel, ".claude/rules/") {
				return "always"
			}
			return "on-demand"
		}
		if dir == "." || rel == ".claude/CLAUDE.md" || rel == ".claude/CLAUDE.local.md" {
			return "always"
		}
		return "on-demand"
	case "agents":
		if dir == "." {
			return "always"
		}
		return "on-demand"
	case "cursor":
		if frontmatterTrue(body, "alwaysApply") {
			return "always"
		}
		return "on-demand"
	case "gemini":
		return "always"
	case "copilot":
		if rel == ".github/copilot-instructions.md" {
			return "always"
		}
		return "on-demand"
	default:
		return "on-demand"
	}
}

func frontmatterHas(body, key string) bool {
	_, ok := frontmatter(body)[strings.ToLower(key)]
	return ok
}

func frontmatterTrue(body, key string) bool {
	v, ok := frontmatter(body)[strings.ToLower(key)]
	if !ok {
		return false
	}
	v = strings.Trim(strings.TrimSpace(v), `"'`)
	return strings.EqualFold(v, "true")
}

func frontmatter(body string) map[string]string {
	out := map[string]string{}
	text := strings.ReplaceAll(body, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return out
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return out
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}

func whenFor(rel, kind, preload, body string) string {
	rel = filepath.ToSlash(rel)
	if preload != "on-demand" {
		return "Every session, before the task starts"
	}
	switch kind {
	case "skill":
		return "Only when this skill is loaded"
	case "claude":
		// Path-scoped rules load when a matching file is read. A subdirectory
		// CLAUDE.md, and a nested .claude/rules directory, load when Claude
		// works in that directory. https://code.claude.com/docs/en/memory
		if strings.Contains("/"+rel, "/.claude/rules/") && frontmatterHas(body, "paths") {
			return "When Claude reads a matching file"
		}
		return "When Claude reads a file in this directory"
	case "agents":
		return "When the session starts here, or Cursor works in this directory"
	case "cursor":
		return "When Cursor attaches this rule"
	case "gemini":
		return "Included in every prompt for a session started at this root"
	case "copilot":
		return "When Copilot works on a matching path"
	default:
		return "Not loaded at the start of every session"
	}
}

func purposeOf(body string) string {
	text := strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "---" {
				break
			}
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "description:"); ok {
				value := strings.Trim(strings.TrimSpace(rest), `"'`)
				if yamlBlock(value) {
					return clipPurpose(firstContinuation(lines, i+1))
				}
				return clipPurpose(value)
			}
		}
	}
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "#") {
			return clipPurpose(strings.TrimSpace(strings.TrimLeft(s, "#")))
		}
	}
	for _, line := range lines {
		s := strings.TrimSpace(line)
		if s != "" && s != "---" {
			return clipPurpose(s)
		}
	}
	return "No description in the file"
}

func yamlBlock(value string) bool {
	switch value {
	case ">", "|", ">-", "|-", ">+", "|+", ">|", "|>":
		return true
	default:
		return false
	}
}

func firstContinuation(lines []string, start int) string {
	for i := start; i < len(lines); i++ {
		s := strings.TrimSpace(lines[i])
		if s == "" || s == "---" {
			if s == "---" {
				return ""
			}
			continue
		}
		return s
	}
	return ""
}

func clipPurpose(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "No description in the file"
	}
	r := []rune(s)
	if len(r) > 140 {
		return string(r[:139]) + "…"
	}
	return s
}

func buildAgents(sources []sourceFile) []Agent {
	order := []string{}
	byID := map[string]*Agent{}
	for _, src := range sources {
		a, ok := byID[src.AgentID]
		if !ok {
			a = &Agent{
				ID:      src.AgentID,
				Name:    src.AgentName,
				Kind:    src.Kind,
				Preload: src.Preload,
				Files:   []FileStat{},
			}
			byID[src.AgentID] = a
			order = append(order, src.AgentID)
		}
		a.Chars += src.Chars
		a.Files = append(a.Files, FileStat{
			Path:    src.Rel,
			Chars:   src.Chars,
			Tokens:  src.Tokens,
			Purpose: src.Purpose,
			When:    src.When,
		})
	}
	agents := make([]Agent, 0, len(order))
	for _, id := range order {
		a := byID[id]
		a.Tokens = a.Chars / 4
		sort.Slice(a.Files, func(i, j int) bool { return a.Files[i].Path < a.Files[j].Path })
		agents = append(agents, *a)
	}
	kindRank := map[string]int{"claude": 0, "agents": 1, "cursor": 2, "gemini": 3, "copilot": 4, "skill": 5}
	sort.Slice(agents, func(i, j int) bool {
		if kindRank[agents[i].Kind] != kindRank[agents[j].Kind] {
			return kindRank[agents[i].Kind] < kindRank[agents[j].Kind]
		}
		if agents[i].Kind == "skill" && agents[i].Tokens != agents[j].Tokens {
			return agents[i].Tokens > agents[j].Tokens
		}
		return agents[i].ID < agents[j].ID
	})
	return agents
}

func duplicateFindings(sources []sourceFile) []Finding {
	type occ struct {
		file  string
		lines []int
	}
	grouped := map[string][]occ{}
	for _, src := range sources {
		perFile := map[string][]int{}
		for i, line := range src.Lines {
			n := strings.TrimSpace(line)
			if ignoreLine(n) {
				continue
			}
			perFile[n] = append(perFile[n], i+1)
		}
		for text, lines := range perFile {
			grouped[text] = append(grouped[text], occ{file: src.Rel, lines: uniqSorted(lines)})
		}
	}
	var out []Finding
	for text, occs := range grouped {
		if len(occs) < 2 {
			continue
		}
		sort.Slice(occs, func(i, j int) bool { return occs[i].file < occs[j].file })
		files := make([]string, 0, len(occs))
		locs := make([]Location, 0, len(occs))
		for _, o := range occs {
			files = append(files, o.file)
			locs = append(locs, Location{File: o.file, Lines: o.lines})
		}
		out = append(out, Finding{
			Kind:      "duplicate_line",
			Message:   fmt.Sprintf("Duplicated across %d files", len(occs)),
			File:      files[0],
			Line:      locs[0].Lines[0],
			Text:      truncateRunes(text, 500),
			Count:     len(occs),
			Files:     files,
			Locations: locs,
		})
	}
	return out
}

func ignoreLine(s string) bool {
	if s == "" || utf8.RuneCountInString(s) < minDuplicateRunes {
		return true
	}
	if strings.Trim(s, " \t`~#*-_=<>|:+.·•") == "" {
		return true
	}
	return false
}

func (s *scanner) pathFindings(sources []sourceFile) []Finding {
	var out []Finding
	seen := map[string]struct{}{}
	for _, src := range sources {
		for i, line := range src.Lines {
			lineNo := i + 1
			refs := extractPaths(line)
			for _, ref := range refs {
				key := src.Rel + "\x00" + fmt.Sprint(lineNo) + "\x00" + ref
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if refExists(s.root, src.Abs, ref) {
					continue
				}
				out = append(out, Finding{
					Kind:    "missing_path",
					Message: "Referenced path does not exist",
					File:    src.Rel,
					Line:    lineNo,
					Path:    ref,
				})
			}
		}
	}
	return out
}

func extractPaths(line string) []string {
	var refs []string
	for _, m := range codeSpanRe.FindAllStringSubmatch(line, -1) {
		if ref, ok := normalizeRef(m[1]); ok && looksLikePath(ref) {
			refs = append(refs, ref)
		}
	}
	for _, m := range mdLinkRe.FindAllStringSubmatch(line, -1) {
		if ref, ok := normalizeRef(m[1]); ok && looksLikePath(ref) {
			refs = append(refs, ref)
		}
	}
	return refs
}

func normalizeRef(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.ContainsAny(s, " \t\n\r") {
		return "", false
	}
	lower := strings.ToLower(s)
	if strings.Contains(lower, "://") || strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(s, "#") {
		return "", false
	}
	if strings.Contains(s, `\`) && (strings.Contains(s, ".") || strings.Count(s, `\`) >= 2) {
		s = strings.ReplaceAll(s, `\`, "/")
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, `"'`)
	s = strings.TrimRight(s, ".,:;)]}")
	s = strings.TrimLeft(s, "([")
	for strings.HasPrefix(s, "./") {
		s = s[2:]
	}
	if s == "" || s == "." || s == ".." || s == "/" {
		return "", false
	}
	if strings.Contains(s, "...") || strings.Contains(s, "//") || strings.ContainsAny(s, `*?|;&<>(){}"',`) {
		return "", false
	}
	if strings.Contains(s, "${") || strings.HasPrefix(s, "$") || strings.Contains(s, ":") || strings.Contains(s, `\`) {
		return "", false
	}
	if utf8.RuneCountInString(s) > 300 {
		return "", false
	}
	return s, true
}

func looksLikePath(s string) bool {
	if s == "" || strings.Contains(s, "//") || strings.HasPrefix(s, "@") {
		return false
	}
	if _, ok := knownExt[strings.ToLower(filepath.Ext(s))]; ok {
		return true
	}
	if !strings.Contains(s, "/") {
		return false
	}
	segs := strings.Split(s, "/")
	real := 0
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		real++
		if !pathSegRe.MatchString(seg) {
			return false
		}
	}
	if real == 0 {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.Contains(s, "/.") || strings.HasPrefix(s, "..") {
		return true
	}
	for _, seg := range segs {
		if strings.Contains(seg, ".") || strings.HasPrefix(seg, ".") {
			return true
		}
	}
	if real >= 3 {
		return true
	}
	first := segs[0]
	if _, ok := pathRoots[first]; ok {
		return true
	}
	return false
}

func refExists(root, sourceAbs, ref string) bool {
	ref = strings.TrimSpace(filepath.ToSlash(ref))
	if ref == "" || strings.HasPrefix(ref, "/") || filepath.IsAbs(filepath.FromSlash(ref)) {
		return false
	}
	ref = filepath.FromSlash(ref)
	for _, c := range []string{
		filepath.Join(root, ref),
		filepath.Join(filepath.Dir(sourceAbs), ref),
	} {
		if entryInside(root, c) {
			return true
		}
	}
	return false
}

// entryInside reports whether full is a directory entry inside root.
// It does not follow symlinks and does not stat a path that leaves root.
func entryInside(root, full string) bool {
	full = filepath.Clean(full)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if rel == "." {
		return true
	}
	cur := root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		next := filepath.Join(cur, part)
		info, err := os.Lstat(next)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return i == len(parts)-1
		}
		if i < len(parts)-1 && !info.IsDir() {
			return false
		}
		cur = next
	}
	return true
}

func (s *scanner) scriptFindings(sources []sourceFile) []Finding {
	var out []Finding
	seen := map[string]struct{}{}
	for _, src := range sources {
		for i, line := range src.Lines {
			lineNo := i + 1
			for _, script := range extractScripts(line) {
				key := src.Rel + "\x00" + fmt.Sprint(lineNo) + "\x00" + script
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				set := s.lookupScripts(src.Abs)
				if set.found && !set.broken && set.scripts[script] {
					continue
				}
				msg := fmt.Sprintf("npm run %q is not defined in package.json", script)
				switch {
				case set.broken:
					msg = fmt.Sprintf("npm run %q is not defined (package.json could not be parsed)", script)
				case !set.found:
					msg = fmt.Sprintf("npm run %q is not defined (no package.json found)", script)
				}
				out = append(out, Finding{
					Kind:        "missing_script",
					Message:     msg,
					File:        src.Rel,
					Line:        lineNo,
					Script:      script,
					PackageFile: set.rel,
				})
			}
		}
	}
	return out
}

func extractScripts(line string) []string {
	var out []string
	for _, m := range npmRunRe.FindAllStringSubmatch(line, -1) {
		script := ""
		switch {
		case m[1] != "":
			script = m[1]
		case m[2] != "":
			script = m[2]
		default:
			script = m[3]
		}
		script = strings.TrimSpace(strings.Trim(script, "`"))
		script = strings.TrimRight(script, ".,:;")
		script = strings.TrimSpace(script)
		if script == "" || strings.HasPrefix(script, "-") {
			continue
		}
		if strings.Contains(script, " ") || scriptNameRe.MatchString(script) {
			out = append(out, script)
		}
	}
	return out
}

func (s *scanner) lookupScripts(fromFile string) scriptSet {
	dir := filepath.Dir(fromFile)
	for {
		if !withinRoot(s.root, dir) {
			return scriptSet{}
		}
		set, ok := s.cache[dir]
		if !ok {
			set = readPackageJSON(s.root, dir)
			s.cache[dir] = set
		}
		if set.found {
			return set
		}
		if samePath(dir, s.root) {
			return scriptSet{}
		}
		parent := filepath.Dir(dir)
		if samePath(parent, dir) {
			return scriptSet{}
		}
		dir = parent
	}
}

func readPackageJSON(root, dir string) scriptSet {
	pkgPath := filepath.Join(dir, "package.json")
	if !withinRoot(root, pkgPath) {
		return scriptSet{}
	}
	info, err := os.Lstat(pkgPath)
	if err != nil {
		return scriptSet{}
	}
	rel, relErr := filepath.Rel(root, pkgPath)
	if relErr != nil {
		rel = pkgPath
	}
	rel = filepath.ToSlash(rel)
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return scriptSet{found: true, broken: true, rel: rel}
	}
	b, err := readLimited(pkgPath, 1<<20)
	if err != nil {
		return scriptSet{found: true, broken: true, rel: rel}
	}
	set := scriptSet{
		found:   true,
		rel:     filepath.ToSlash(rel),
		scripts: map[string]bool{},
	}
	var parsed struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		set.broken = true
		return set
	}
	for name := range parsed.Scripts {
		set.scripts[name] = true
	}
	return set
}

func oversizedFindings(sources []sourceFile) []Finding {
	var out []Finding
	for _, src := range sources {
		if src.Tokens <= oversizedTokenLimit {
			continue
		}
		out = append(out, Finding{
			Kind:    "oversized_file",
			Message: fmt.Sprintf("File is over the %d token preload limit", oversizedTokenLimit),
			File:    src.Rel,
			Tokens:  src.Tokens,
			Chars:   src.Chars,
		})
	}
	return out
}

func sortFindings(findings []Finding) {
	kindRank := map[string]int{
		"duplicate_line": 0,
		"missing_path":   1,
		"missing_script": 2,
		"oversized_file": 3,
	}
	sort.Slice(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if kindRank[a.Kind] != kindRank[b.Kind] {
			return kindRank[a.Kind] < kindRank[b.Kind]
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Script != b.Script {
			return a.Script < b.Script
		}
		return a.Text < b.Text
	})
}

func withinRoot(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func uniqSorted(in []int) []int {
	if len(in) == 0 {
		return in
	}
	sort.Ints(in)
	out := in[:1]
	for _, n := range in[1:] {
		if n != out[len(out)-1] {
			out = append(out, n)
		}
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 4 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}
