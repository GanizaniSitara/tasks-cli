# Ticket recall hook

`tasks_hook_inject.py` gives a coding agent the tickets relevant to whatever you
just asked it, without you having to remember to search.

## Why

Agents reconstruct context from whatever is in front of them. If the ticket
corpus is not in front of them, they reconstruct it from memory, from the code,
or from assumption — and confidently rebuild something you already designed and
wrote down. The cost is not a wrong answer so much as a wasted detour.

This hook runs `tasks-cli search` against each prompt and injects the matches as one
line each: id, status, title, and the `codebase:` path when the ticket declares
one. Tickets are pointers. The agent runs `tasks-cli get <ID>` when a pointer looks
worth following, so the per-turn cost stays flat no matter how long the matching
tickets are.

## Install

Requires `tasks-cli` on `PATH` (or set `TASKS_CLI` to its full path) and Python 3.9+.

Claude Code — add to `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "python3 /path/to/hooks/tasks_hook_inject.py --agent claude",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
```

On Windows, use the full interpreter path and forward slashes. Hooks load at
startup, so restart the agent afterwards.

Verify it without an agent by feeding it an event on stdin:

```sh
echo '{"hook_event_name":"UserPromptSubmit","session_id":"t","prompt":"flaky login test"}' \
  | python3 tasks_hook_inject.py --agent claude
```

Empty output means nothing matched — which is a valid answer, not a failure.

## Behaviour

- **Read-only.** It only ever runs `tasks-cli search`.
- **Fail-open.** Any error, timeout, malformed input or empty result yields empty
  stdout and exit 0. A prompt is never blocked by this hook.
- **Bounded.** At most 5 tickets per turn and 60 lines per session, so a long
  conversation cannot silently accrete ticket noise.
- **Deduped.** A ticket injected once is not injected again in that session.
- **Quiet on filler.** Acknowledgements ("ok", "thanks"), bare slash commands and
  repeated prompts skip the search entirely.

## Relevance

The hard part is not retrieval, it is refusing to answer. A full-text index
always returns its nearest matches, so a prompt about buying coffee will happily
surface a DNS ticket if nothing better exists.

Two guards:

1. `tasks-cli search` reports whether it matched **every** query term (`bleve`) or
   fell back to any-term matching (`bleve-relaxed`).
2. A relaxed answer is filtered by how many query words each ticket actually
   contains. That floor scales with the query: two shared words mean nothing in
   a two-word prompt and quite a lot in a wordy one.

Prompts are reduced to their salient words first, because searching a whole
dictated sentence guarantees the strict pass fails and drops everything into the
relaxed path.

Telemetry — counts and timings only, never prompt or ticket text — is appended to
`tasks-hook.jsonl` under the platform log directory. Inspect the `gate` field
there to see why a given turn injected nothing.
