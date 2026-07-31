# tasks-cli

`tasks` is a local Go command-line interface for a markdown-backed task corpus.
It runs on Windows and macOS from the same source.

Markdown remains the source of truth. Bleve is a derived local search index, updated after every CLI mutation so search is a pure index read.

The command prints JSON so coding agents can use it safely from a shell.

## Commands

`summary`, `projects`, `search`, `get`, `create`, `update`, `move`, `reopen`, `delete`, `duplicates`, `note`, `attach`, `lint`, `pivot`, `repair`, `migrate`, and `index sync|rebuild`.

`tasks help` lists the commands. `tasks <command> --help` (or `tasks help <command>`)
prints that command's flags, defaults, and an example — this works without a
readable config, so it is safe to probe on a fresh machine.

There is no `list` command: `tasks search` with no query lists tasks from disk,
so `tasks search --status in-progress` is the way to enumerate a status.

## Build and use

```powershell
go build -o tasks.exe .\cmd\tasks
.\tasks.exe index rebuild
.\tasks.exe search "wine scraper" --limit 5
.\tasks.exe move PROJ-092 done
.\tasks.exe note PROJ-092 --note "Verified before closing."
```

```sh
go build -o tasks ./cmd/tasks
./tasks index rebuild
./tasks search "wine scraper" --limit 5
```

Flags may appear before or after positional arguments. Repeat `--tag` for
multiple tags. Mutations take a short corpus lock and update the Bleve index
before returning. `delete` requires `--confirm TASK-ID`; `repair` and `migrate`
are dry-runs unless given `--apply`.

`update --title` rewrites the title in frontmatter but leaves the filename
alone, so links stay valid. `tasks migrate` reconciles file stems with titles.
If the legacy Python service or another non-CLI writer changes the corpus, run
`tasks index sync` before the next indexed search.

## Configuration

By default the command reads `tasks-cli/config.yaml` under the per-user config
directory and uses:

| | Windows | macOS / Linux |
|---|---|---|
| config | `%APPDATA%\\tasks-cli\\` | `~/.config/tasks-cli/` |
| index | `%LOCALAPPDATA%\\tasks-cli\\bleve` | `~/.local/share/tasks-cli/bleve` |
| corpus | `%USERPROFILE%\\tasks` | `~/tasks` |

The prefix allowlist (`allowed_prefixes.yaml`) sits beside the config file.

`TASKS_ROOT`, `TASKS_CONFIG`, and `TASKS_INDEX_DIR` override these locations.

The repository contains no personal project prefixes. Copy the existing allowlist into the configured location during deployment.

## Agent integration

`hooks/` contains a `UserPromptSubmit` hook that searches the corpus on every
prompt and injects the matching tickets as one-line pointers, so an agent stops
rebuilding things you already specified. See `hooks/README.md`.
