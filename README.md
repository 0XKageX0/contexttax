# contexttax

`contexttax` is a single Go binary that tells you what your coding agents pay to read a repository before they do any work.

Run it at the root of a repo. It finds the instruction files those agents can load, estimates the token cost, and points at the waste: duplicated lines, paths that do not exist, `npm run` scripts missing from `package.json`, and any file over 2,500 estimated tokens.

## Install

Go 1.24 or newer.

```bash
go install github.com/0XKageX0/contexttax@latest
```

Or build from a clone:

```bash
git clone https://github.com/0XKageX0/contexttax.git
cd contexttax
go build -o contexttax .
./contexttax
```

On Windows, from the clone:

```bat
go build -o contexttax.exe .
contexttax.exe
contexttax.exe C:\path\to\repo
```

Do not use `./contexttax` in Command Prompt. That form is for macOS and Linux.

A published `v*` tag includes `contexttax_<version>_<os>_<arch>` (`.exe` on Windows) and `SHA256SUMS`. You do not need Go to run that exe. From the directory you want to scan:

```bat
contexttax_0.2.2_windows_amd64.exe
contexttax_0.2.2_windows_amd64.exe C:\path\to\repo
```

## Usage

```bash
contexttax                 # scan the current directory, listen on 127.0.0.1:7070
contexttax /path/to/repo
contexttax -addr :8080 .   # rewritten to 127.0.0.1:8080
```

Open <http://127.0.0.1:7070>. The program opens that page in your browser. The page is embedded in the binary. There is no Node server and no frontend framework.

The process listens on `127.0.0.1` only. Wildcard and loopback hosts are rewritten to `127.0.0.1` with the same port, including `-addr :7070`, `-addr 0.0.0.0:7070`, `-addr [::]:7070`, `-addr localhost:7070`, and `-addr [::1]:7070`. Any other host is rejected.

Each page load, and each `GET /api/report`, scans the tree again. A folder the process cannot read is skipped. It does not fail the scan.

## What it reads

The scan walks the directory you pass. Labels below are for a session started in that directory.

| File | Agent | When you pay |
| --- | --- | --- |
| `CLAUDE.md` and `CLAUDE.local.md` in the scan root, and `.claude/CLAUDE.md` | Claude Code | Every session. [Docs](https://code.claude.com/docs/en/memory) |
| `CLAUDE.md` or `CLAUDE.local.md` in a subdirectory | Claude Code | On demand, when Claude reads, writes, or edits a file in that directory |
| `.claude/rules/**/*.md` with no `paths` field | Claude Code | Every session. Subfolders of that rules directory are included |
| `.claude/rules/**/*.md` with a `paths` field | Claude Code | On demand, when Claude reads a matching file |
| `**/.claude/rules/**/*.md` under a subdirectory | Claude Code | On demand. A nested `.claude/rules/` directory is not the project rules tree |
| `AGENTS.md` or `AGENTS.override.md` in the scan root | Codex and Cursor | Every session. [Codex](https://developers.openai.com/codex/guides/agents-md), [Cursor](https://cursor.com/docs/rules) |
| `AGENTS.md` or `AGENTS.override.md` in a subdirectory | Codex and Cursor | Not loaded when the session starts at the repo root. Codex includes it when the session starts in that directory or a child of it. Cursor applies it when working on files in that directory or below |
| `.cursor/rules/**/*.mdc` with `alwaysApply: true` | Cursor | Every session. Nested rule folders are included |
| Other `.cursor/rules/**/*.mdc` | Cursor | On demand: matching globs, the description, or an `@` mention |
| `GEMINI.md` anywhere under the root | Gemini CLI | Every prompt. Subdirectory files are concatenated in, not loaded later. [Docs](https://google-gemini.github.io/gemini-cli/docs/cli/gemini-md.html) |
| `.github/copilot-instructions.md` | GitHub Copilot | Every request in the repository. [Docs](https://docs.github.com/en/copilot/customizing-copilot/adding-custom-instructions-for-github-copilot) |
| `.github/instructions/**/*.instructions.md` | GitHub Copilot | On demand, when Copilot works on a matching path |

Codex reads at most one of `AGENTS.override.md` and `AGENTS.md` per directory, preferring the override. The scan lists both when both exist, so the cost of each file stays visible.

`.cursorrules` is not scanned. The current Cursor rules page does not list it.

Skipped directories, matched case-insensitively: `node_modules`, `.git`, `vendor`, `dist`, `.next`, `coverage`, `.turbo`, `.cache`, `.tools`, `.contexttax`, `appdata`, `application data`, `local settings`, `$recycle.bin`, `system volume information`, `windows`, `program files`, `program files (x86)`, `programdata`, `recovery`.

## The bill

The estimate is Unicode code points divided by 4, rounded down. It is not a model tokenizer. A file that contains a NUL byte, and any file larger than 4 MiB, uses the byte length divided by 4 instead, and line findings are skipped for that file.

An agent's total divides the summed character count once, so it can be a few tokens higher than the sum of the per-file rows.

**Always-on** is the cost when you start the agent in the scanned directory. **On demand** is everything else in the table. Those files are not all in context at launch.

## Limitations

- Parent directories are not scanned. User-level instruction files are not scanned either, including `~/.claude/CLAUDE.md`, `~/.codex`, and `~/.gemini/GEMINI.md`, unless that directory is the root you passed.
- The token count is characters divided by 4. It undercounts CJK and other non-Latin text, because one character is often more than a quarter of a token.

## Findings

| Kind | Rule |
| --- | --- |
| `duplicate_line` | The same trimmed line, at least 20 characters, appears in two or more instruction files |
| `missing_path` | A backtick span or Markdown link looks like a file path, and that path exists neither beside the file nor at the repo root |
| `missing_script` | The text says `npm run X`, and `X` is not a script in the nearest `package.json` |
| `oversized_file` | One file estimates to more than 2,500 tokens. Exactly 2,500 is allowed |

A line repeated only inside one file is not a duplicate. URLs, globs, and `@/` import aliases are not treated as missing files.

Each file on the dashboard shows where it lives, when that agent loads it, and a purpose line taken from its own `description` or first heading.

## Cutting a file off

**Cut off** asks for a second click, then copies that instruction file into `.contexttax/held`, writes `.contexttax/held.json`, and deletes the original. **Allow again** also asks for a second click, then puts the parked bytes back.

If a file is already at the original path, Allow again refuses and leaves the parked copy where it is. If the process dies after the copy and the manifest write, the next scan deletes the original only when those two copies are byte for byte the same. A different file at the original path is left alone.

Agents load these files by name, so a parked file is no longer on the path they search. This is a permission you give from the dashboard. It is not a sandbox. If you paste the parked path into a chat, the agent can still open that copy. Commit `.contexttax/held` if the cutoff should stay off for everyone who clones the repo.

`POST /api/hold` and `POST /api/release` reject a cross-origin browser request. A request with no `Origin` header, such as curl on the same machine, is still accepted.

## API

`GET /api/report` returns JSON:

- `agents` — id, name, kind (`claude`, `agents`, `cursor`, `gemini`, `copilot`, `skill`), `preload` (`always` or `on-demand`), characters, tokens, and files. Each file has `path`, `purpose`, `when`, `chars`, and `tokens`. An on-demand file that is not a skill uses an id ending in `:scoped`, so it is not added to the always-on row
- `held` — files parked by Cut off, with the same fields plus `heldAt`
- `totals` — file and agent counts, plus always-on, on-demand, held, and combined characters and tokens
- `findings` — kind, message, and the locations that produced it
- `tokenLimit` — `2500`
- `root` — absolute path of the scan
- `scannedAt` — time of that request

`POST /api/hold` and `POST /api/release` take `{"path":"skills/review/SKILL.md"}` and return the new report. The path must be relative to the scanned root. Absolute paths, drive letters, `..`, and symlinks are rejected.

## Development

```bash
go test ./...
```

[`testdata/repo`](testdata/repo) is a fixture. It contains a duplicated instruction, missing paths, missing npm scripts, and a skill file over the token limit. The `node_modules` file in that fixture exists so the skip rule can be checked. It is not a dependency. `testdata/repo/.cursor/rules/nested/extra.mdc` is a nested Cursor rule and is counted as on demand. `testdata/repo/.cursor/rules/style.mdc` has a description and no `alwaysApply: true`, so it is on demand too.

## License

[MIT](LICENSE)
