# contexttax

`contexttax` is a single Go binary that tells you what your coding agents pay to read a repository before they do any work.

Run it at the root of a repo. It finds the instruction files those agents preload, estimates the token cost, and points at the waste: duplicated lines, paths that do not exist, `npm run` scripts missing from `package.json`, and any file over 2,500 estimated tokens.

## Install

Go 1.24 or newer.

```bash
go install github.com/0XKage0X/contexttax@latest
```

Or build this repository:

```bash
git clone https://github.com/0XKage0X/contexttax.git
cd contexttax
go build -o contexttax .
./contexttax
```

## Usage

```bash
contexttax                 # scan the current directory, listen on :7070
contexttax /path/to/repo
contexttax -addr :8080 .
```

Open <http://127.0.0.1:7070>. The program opens that page in your browser. The page is the interface. It is embedded in the binary. There is no Node server and no frontend framework.

On Windows, download `contexttax_0.2.1_windows_amd64.exe` from the releases page. You do not need Go installed. In Command Prompt, from the repository you want to scan:

```bat
contexttax_0.2.1_windows_amd64.exe
```

Or point it at a folder:

```bat
contexttax_0.2.1_windows_amd64.exe C:\path\to\repo
```

Do not use `./contexttax`. That is a macOS and Linux command. The clone address is `https://github.com/0XKageX0/contexttax.git`.

Each page load, and each `GET /api/report`, scans the tree again. A folder Windows will not let the program read is skipped. It does not fail the scan.

## What it reads

| File | Agent | When you pay |
| --- | --- | --- |
| `CLAUDE.md` | Claude | Always, anywhere in the tree |
| `AGENTS.md` | Agents | Always, anywhere in the tree |
| `.cursor/rules/*.mdc` | Cursor | Always. Nested rule folders are not included |
| `**/SKILL.md` | One agent per skill directory | On demand |

Skipped directories: `node_modules`, `.git`, `vendor`, `dist`, `.next`, `coverage`, `.turbo`, `.cache`, `.tools`.

## The bill

Tokens are `character count / 4`, rounded down. That is a stable estimate, not a model tokenizer. An agent's total divides the combined text once, so it can be a few tokens higher than the sum of the per-file rows.

Claude, Agents, and Cursor are **always-on**: that cost is in context before the task starts. Each skill is **on demand**: you pay it only when that skill is loaded.

## Findings

| Kind | Rule |
| --- | --- |
| `duplicate_line` | The same trimmed line, at least 20 characters, appears in two or more instruction files |
| `missing_path` | A backtick span or Markdown link looks like a file path, and that path exists neither beside the file nor at the repo root |
| `missing_script` | The text says `npm run X`, and `X` is not a script in the nearest `package.json` |
| `oversized_file` | One file estimates to more than 2,500 tokens. Exactly 2,500 is allowed |

A line repeated only inside one file is not a duplicate. URLs, globs, and `@/` import aliases are not treated as missing files.

Each file on the dashboard also shows where it lives, when that agent loads it, and a purpose line taken from its own `description` or first heading. Tokens stay an estimate: characters divided by 4, not a model tokenizer. This is the cost of preloading the file, not a meter of one live chat.

## Cutting a file off

**Cut off** moves that instruction file into `.contexttax/held` and remembers the original path. Claude, Cursor, and other agents load `CLAUDE.md`, `AGENTS.md`, `.cursor/rules/*.mdc`, and `SKILL.md` by those names, so they no longer pick the file up. **Allow again** puts the same bytes back.

This is a permission you give from the dashboard. It is not a sandbox. If you paste the parked path into a chat, the agent can still open that copy. Commit `.contexttax/held` if the cutoff should stay off for everyone who clones the repo.

## API

`GET /api/report` returns JSON:

- `agents` — id, name, kind (`claude`, `agents`, `cursor`, `skill`), `preload` (`always` or `on-demand`), characters, tokens, and files. Each file has `path`, `purpose`, `when`, `chars`, and `tokens`
- `held` — files parked by Cut off, with the same fields plus `heldAt`
- `totals` — file and agent counts, plus always-on, on-demand, held, and combined characters and tokens
- `findings` — kind, message, and the locations that produced it
- `tokenLimit` — currently `2500`
- `scannedAt` — time of that request

`POST /api/hold` and `POST /api/release` take `{"path":"skills/review/SKILL.md"}` and return the new report.

## Development

```bash
go test ./...
```

[`testdata/repo`](testdata/repo) is a fixture. It contains a duplicated instruction, missing paths, missing npm scripts, and a skill file over the token limit. The `node_modules` file in that fixture exists so the skip rule can be checked. It is not a dependency.

## License

[MIT](LICENSE)
