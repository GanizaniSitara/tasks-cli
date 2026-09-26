# tasks-cli

`tasks-cli` is a local Go command-line interface for a markdown-backed task corpus.
It runs on Windows and macOS from the same source.

The command is deliberately named `tasks-cli` rather than `tasks`. A coding agent
meeting a bare `tasks` on an unfamiliar machine confuses it with the OS task
scheduler, an MCP server, or its own todo list; the hyphenated name is
self-identifying wherever it appears.

Markdown remains the source of truth. Bleve is a derived local search index, updated after every CLI mutation so search is a pure index read.

The command prints JSON so coding agents can use it safely from a shell.

## Commands

`summary`, `projects`, `search`, `next`, `get`, `create`, `update`, `move`, `reopen`, `delete`, `defer`, `decline`, `duplicates`, `dedupe`, `note`, `attach`, `asset add|update|remove|list`, `lint`, `pivot`, `repair`, `migrate`, `backfill`, and `index sync|rebuild`.

Tickets and their companion assets each get a full set of verbs: `create`/`update`
for the ticket body, `asset add`/`asset update`/`asset remove` for the files beside
it. `attach` remains as an alias for `asset add`. Because direct filesystem edits to
the corpus are typically denied so the CLI stays the only writer, an asset that could
only be created and never revised was effectively write-once; `asset update` closes
that. Replacements and removals report `previous_size` and `previous_sha256` so the
change is auditable from the JSON alone.

`tasks-cli help` lists the commands. `tasks-cli <command> --help` (or
`tasks-cli help <command>`) prints that command's flags, defaults, and an example —
this works without a readable config, so it is safe to probe on a fresh machine.

There is no `list` command: `tasks-cli search` with no query lists tasks from disk,
so `tasks-cli search --status in-progress` is the way to enumerate a status.

## What is actionable, versus what exists

`search` enumerates by priority then task id and nothing else, which answers "what
is in the corpus". It does not answer "what should I do now": the same head leads
the list every day until a status changes, so rejections get replayed, work that is
waiting on somebody else is indistinguishable from work you could start this
minute, and tickets that can only be done on another machine still appear.

`next` answers the second question, using four optional frontmatter fields:

| field | meaning |
| --- | --- |
| `defer_until: YYYY-MM-DD` | hidden until that date, then returns by itself |
| `declined_until: YYYY-MM-DD` | "not this one" — set by `decline`, expires |
| `waiting_on: me \| <party>` | anything but `me` is somebody else's move |
| `context: [desktop, terminal, mac]` | where the work can physically happen |

```
tasks-cli next --context desktop     # local workstation / desktop
tasks-cli next --context terminal    # remote work (e.g. from office / console)
tasks-cli defer EYE-022 --until 2026-09-26 --because "waiting on seller replies"
tasks-cli decline OP-249 --for 14d --because "not this fortnight"
```

Every field is optional and absence preserves the old behaviour, so nothing has to
be migrated before `next` is useful. A task that declares no context matches every
context — unlabelled work could be doable anywhere, and hiding it would lose it.
The `suppressed` counts in the output say how many tasks were hidden and why, so an
empty list is never ambiguous about whether you are finished or merely filtered.

Within a priority band, ties break by staleness (oldest `updated` first) so the head
of the list rotates; `--warm` flips it to freshest first.

`backfill` converts existing conventions into context data — titles containing
"(AT WORK)" become `context: terminal`, and `--mac-prefix CSF --mac-prefix GOL` marks
those prefixes `context: mac`. Dry run unless `--apply`.

## Install

Each host builds its own binary from this one source. Install through the
scripts so every host puts it in the same place and stamps it with the commit
it came from; a hand-built copy left beside the source is on nobody's PATH,
easy to run by accident, and ages silently.

```powershell
.\scripts\install.ps1              # tests, builds, installs to C:\Tools\tasks-cli.exe
```

```sh
./scripts/install.sh               # tests, builds, installs to ~/.local/bin/tasks-cli
```

Both take an alternative destination as their first argument, and both finish
by printing `tasks-cli version`. Compare that across hosts to spot one that has
fallen behind: it reports the commit, the build time, and the file on disk that
answered.

## Use

```powershell
tasks-cli index rebuild
tasks-cli search "wine scraper" --limit 5
tasks-cli move PROJ-092 done
tasks-cli note PROJ-092 --note "Verified before closing."
```

Flags may appear before or after positional arguments. Repeat `--tag` for
multiple tags. Mutations take a short corpus lock and update the Bleve index
before returning. `delete` requires `--confirm TASK-ID`; `repair` and `migrate`
are dry-runs unless given `--apply`.

`update --title` rewrites the title in frontmatter but leaves the filename
alone, so links stay valid. `tasks-cli migrate` reconciles file stems with titles.
If the legacy Python service or another non-CLI writer changes the corpus, run
`tasks-cli index sync` before the next indexed search.

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
