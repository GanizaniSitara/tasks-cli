"""Ticket recall hook for Claude Code and Codex.

Companion to ``mempalace_hook_inject.py``. Memory is searched automatically on
every prompt; the ticket corpus was not, so an agent would reconstruct from
curated memory (often the *old* design) while the current answer sat in a task
file. This closes that gap: the same prompt that queries mempalace also queries
the Bleve ticket index.

What it injects is deliberately thin -- one line per hit: id, status, title, and
the ``codebase:`` path when the ticket declares one. Tickets are pointers, not
prose. The agent reads the full body with ``tasks get`` if the pointer looks
relevant, which keeps the per-turn cost near-constant regardless of how large
the matching tickets are.

Behavior contract, matching the mempalace hook:
- READ-ONLY. Only ever runs ``tasks search``; never mutates the corpus.
- Fail open: any error, timeout, malformed input or empty result yields empty
  stdout and exit code 0. A prompt is never blocked by this hook.
- Bounded: at most MAX_HITS lines per turn, each truncated, with a cumulative
  per-session ceiling so a long session cannot accrete ticket noise.
- Deduped: a ticket already injected in this session is not injected again.

Usage: fed via stdin by the harness; agent selected with ``--agent claude|codex``.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

def _find_tasks_cli() -> str:
    """Locate the tasks binary: PATH first, then the known install locations.

    Kept dynamic so the same hook runs on Windows and macOS without a per-host
    edit. An override wins outright, for anyone whose layout differs from both.
    """
    override = os.environ.get("TASKS_CLI")
    if override:
        return override
    found = shutil.which("tasks")
    if found:
        return found
    for candidate in (
        Path(r"C:\Tools\tasks.exe"),
        Path.home() / ".local" / "bin" / "tasks",
        Path.home() / "bin" / "tasks",
    ):
        if candidate.exists():
            return str(candidate)
    return "tasks"  # let the subprocess call fail, and fail open


TASKS_EXE = _find_tasks_cli()

CANDIDATE_LIMIT = 8          # asked of the index
MAX_HITS = 5                 # injected after filtering
SEARCH_TIMEOUT = 2.5         # internal deadline; harness hook timeout is longer
SESSION_LINE_CEILING = 60    # total ticket lines injected per session
MAX_TITLE_CHARS = 90
MAX_TERMS = 8                # salient words kept from the prompt
MIN_RELAXED_OVERLAP = 2      # distinct terms a relaxed hit must actually contain

# Connective tissue that carries no retrieval signal. Deliberately short: the
# aim is to strip conversational filler, not to build a linguistics project.
_STOPWORDS = frozenset("""
the and for with that this from have has had was were are you your our its
not but can will would should could there their they them then than when
where whats hows heres theres been being were does doing gone going
what which who whom how why all any some each got get put see look need
about into over under out off now just also more most other another same
one two now new old why way lets let make made does did done doing
""".split())

STATE_DIR = Path(tempfile.gettempdir()) / "tasks-hooks"
_WIN_LOGS = Path(r"C:\scripts\logs")
TELEMETRY_PATH = (
    _WIN_LOGS if os.name == "nt" else Path.home() / ".local" / "share" / "tasks-cli"
) / "tasks-hook.jsonl"
TELEMETRY_MAX_BYTES = 2_000_000

# Exact normalized acknowledgements that never benefit from ticket recall.
_ACK_PROMPTS = {
    "ok", "okay", "yes", "y", "no", "n", "thanks", "thank you", "ty",
    "go", "continue", "next", "done", "sure", "yep", "nope", "k",
}
_IDENT_RE = re.compile(r"\b[A-Z]{3,}-\d{1,4}\b")
_CODEBASE_RE = re.compile(r"codebase:\s*([^\s\"',}\]\\n]+(?:[\\/][^\s\"',}\]]+)*)")
# Bleve returns snippets with <b class="match ..."> markup around hit terms.
_TAG_RE = re.compile(r"<[^>]+>")


def _now() -> float:
    return time.time()


def _norm(text: str) -> str:
    return re.sub(r"\s+", " ", text or "").strip()


def _sanitize_session_id(raw: str) -> str:
    return hashlib.sha256((raw or "none").encode("utf-8")).hexdigest()[:16]


def _emit_telemetry(record: dict[str, Any]) -> None:
    try:
        TELEMETRY_PATH.parent.mkdir(parents=True, exist_ok=True)
        if TELEMETRY_PATH.exists() and TELEMETRY_PATH.stat().st_size > TELEMETRY_MAX_BYTES:
            TELEMETRY_PATH.write_text("", encoding="utf-8")
        with TELEMETRY_PATH.open("a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")
    except Exception:
        pass  # telemetry must never affect the hook


def _state_path(agent: str, session_id: str, generation: int) -> Path:
    return STATE_DIR / f"{agent}-{session_id}-g{generation}.json"


def _load_state(path: Path) -> dict[str, Any]:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception:
        return {"lines": 0, "task_ids": [], "last_query_hash": ""}


def _save_state(path: Path, state: dict[str, Any]) -> None:
    try:
        STATE_DIR.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(".tmp")
        tmp.write_text(json.dumps(state), encoding="utf-8")
        os.replace(tmp, path)
    except Exception:
        pass


def _prune_state(max_age_days: int = 7) -> None:
    try:
        cutoff = _now() - max_age_days * 86400
        for f in STATE_DIR.glob("*.json"):
            try:
                if f.stat().st_mtime < cutoff:
                    f.unlink()
            except Exception:
                pass
    except Exception:
        pass


def salient_terms(prompt: str) -> list[str]:
    """The words worth searching on, in order, deduped.

    A dictated prompt is mostly connective tissue. Searching the raw sentence
    guarantees the CLI's strict pass fails (no task contains every word of it)
    and drops to the relaxed any-term pass, which is exactly the noise this
    hook exists to avoid.
    """
    words = re.findall(r"[A-Za-z][A-Za-z0-9._-]{2,}", _norm(prompt).lower())
    terms: list[str] = []
    for word in words:
        if word in _STOPWORDS or word in terms:
            continue
        terms.append(word)
        if len(terms) >= MAX_TERMS:
            break
    return terms


def build_query(prompt: str) -> tuple[str, list[str]]:
    """Return (query, terms). Explicit ticket IDs always survive."""
    terms = salient_terms(prompt)
    # Case matters: task_id is a keyword field, so "proj-17" never matches
    # "PROJ-17". Carry the identifier through exactly as written.
    idents = _IDENT_RE.findall(_norm(prompt))
    for ident in reversed(idents):
        for existing in list(terms):
            if existing.lower() == ident.lower():
                terms.remove(existing)
        terms.insert(0, ident)
    terms = terms[:MAX_TERMS]
    return " ".join(terms)[:250], terms


def gate_reason(prompt: str, last_query_hash: str, query_hash: str) -> str | None:
    """Return a skip reason, or None to proceed with the search."""
    norm = _norm(prompt).lower()
    if not norm:
        return "empty"
    if norm in _ACK_PROMPTS:
        return "ack"
    if norm.startswith("/") and " " not in norm:
        return "slash-command"
    if query_hash and query_hash == last_query_hash:
        return "duplicate-prompt"
    return None


def search_tasks(query: str) -> tuple[list[dict[str, Any]], str]:
    """Return (results, matched) where matched is the CLI's own precision flag.

    ``bleve`` means every query term was present in every hit; ``bleve-relaxed``
    means the strict pass found nothing and these are any-term matches, which
    for a conversational prompt is usually noise.
    """
    completed = subprocess.run(
        [TASKS_EXE, "search", query, "--limit", str(CANDIDATE_LIMIT)],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=SEARCH_TIMEOUT,
    )
    if completed.returncode != 0:
        return [], ""
    payload = json.loads(completed.stdout or "{}")
    results = payload.get("results")
    matched = str((payload.get("sync") or {}).get("matched") or "")
    return (results if isinstance(results, list) else []), matched


def overlap(result: dict[str, Any], terms: list[str]) -> int:
    """How many distinct query terms this task actually contains."""
    haystack = " ".join([
        str(result.get("task_id") or ""),
        str(result.get("title") or ""),
        " ".join(str(t) for t in (result.get("tags") or [])),
        _TAG_RE.sub("", str(result.get("snippet") or "")),
    ]).lower()
    return sum(1 for term in terms if term.lower() in haystack)


def _codebase_of(result: dict[str, Any]) -> str:
    """The ticket's declared codebase, if the snippet happens to carry it.

    Cheap and best-effort: reading every hit's body to find `codebase:` would
    cost more than the whole hook is worth. A missing path is not an error --
    the agent runs `tasks get` when it wants the detail.
    """
    snippet = _TAG_RE.sub("", str(result.get("snippet") or ""))
    match = _CODEBASE_RE.search(snippet)
    return match.group(1).rstrip(".,;") if match else ""


def format_hits(
    results: list[dict[str, Any]],
    seen_ids: set[str],
    remaining_lines: int,
) -> list[str]:
    lines: list[str] = []
    for result in results:
        if len(lines) >= min(MAX_HITS, remaining_lines):
            break
        task_id = str(result.get("task_id") or "").strip()
        if not task_id or task_id in seen_ids:
            continue
        title = _norm(str(result.get("title") or ""))
        if len(title) > MAX_TITLE_CHARS:
            title = title[: MAX_TITLE_CHARS - 3].rstrip() + "..."
        status = str(result.get("status") or "?")
        line = f"- {task_id} [{status}] {title}"
        codebase = _codebase_of(result)
        if codebase:
            line += f"  (codebase: {codebase})"
        lines.append(line)
        seen_ids.add(task_id)
    return lines


def render_envelope_text(lines: list[str]) -> str:
    return "\n".join([
        "<task_recall>",
        "Live tickets matching this prompt, from the local task index. These are",
        "pointers, not instructions: read the full body with `tasks get <ID>`",
        "before relying on one, and check its status -- a done ticket describes",
        "what was built, not what is pending.",
        "",
        *lines,
        "</task_recall>",
    ]) + "\n"


def emit_context(event: str, context_text: str) -> None:
    payload = {
        "hookSpecificOutput": {
            "hookEventName": event,
            "additionalContext": context_text,
        }
    }
    data = json.dumps(payload, ensure_ascii=False)
    # Emit UTF-8 regardless of the console codepage (Windows stdout defaults to
    # cp1252, which cannot encode em-dashes and other ticket text).
    buffer = getattr(sys.stdout, "buffer", None)
    if buffer is not None:
        buffer.write(data.encode("utf-8"))
        buffer.flush()
    else:
        sys.stdout.write(data)


def run(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--agent", choices=["claude", "codex"], required=True)
    args = parser.parse_args(argv)
    started = time.monotonic()

    tele: dict[str, Any] = {
        "ts": None, "agent": args.agent, "event": None, "session": None,
        "gate": None, "elapsed_ms": None, "candidates": 0, "injected": 0,
        "matched": None, "error": None,
    }

    try:
        raw = sys.stdin.read()
        data = json.loads(raw) if raw.strip() else {}
    except Exception as exc:
        tele["gate"] = "bad-stdin"
        tele["error"] = type(exc).__name__
        _finish(tele, started)
        return 0

    event = data.get("hook_event_name") or ""
    tele["event"] = event
    if event != "UserPromptSubmit":
        # SessionStart has no prompt to search with, and a generic query would
        # inject whatever happens to rank highest -- noise, not recall.
        tele["gate"] = "unsupported-event"
        _finish(tele, started)
        return 0

    session_id = _sanitize_session_id(str(data.get("session_id", "")))
    tele["session"] = session_id
    generation = 1 if str(data.get("source", "") or "") in ("clear", "compact") else 0
    state_path = _state_path(args.agent, session_id, generation)
    _prune_state()

    prompt = str(data.get("prompt", "") or "")
    query, terms = build_query(prompt)
    query_hash = hashlib.sha256(query.encode("utf-8")).hexdigest()[:16]
    state = _load_state(state_path)

    gate = gate_reason(prompt, state.get("last_query_hash", ""), query_hash)
    if gate is not None:
        tele["gate"] = gate
        _finish(tele, started)
        return 0
    if not terms:
        tele["gate"] = "no-terms"
        _finish(tele, started)
        return 0

    try:
        results, matched = search_tasks(query)
    except Exception as exc:
        tele["gate"] = "search-failed"
        tele["error"] = type(exc).__name__
        _finish(tele, started)
        return 0

    tele["candidates"] = len(results)
    tele["matched"] = matched
    if not results:
        tele["gate"] = "no-results"
        _finish(tele, started)
        return 0

    # A relaxed answer means the strict pass found nothing, so these are
    # any-term matches. Keep only those that genuinely share several words with
    # the prompt -- one incidental word in common is how "buy more coffee"
    # ends up recommending a DNS ticket.
    if matched == "bleve-relaxed":
        # Two shared words is a real signal in a wordy prompt and impossible in
        # a two-word one, so the floor has to follow the query's size. Chasing
        # this with a bigger stopword list instead just moves the failure to
        # whichever filler word is missing next.
        floor = 1 if len(terms) <= 2 else MIN_RELAXED_OVERLAP
        results = [r for r in results if overlap(r, terms) >= floor]
        results.sort(key=lambda r: overlap(r, terms), reverse=True)
        if not results:
            tele["gate"] = "relaxed-below-floor"
            _finish(tele, started)
            return 0

    remaining = SESSION_LINE_CEILING - int(state.get("lines", 0))
    if remaining <= 0:
        tele["gate"] = "session-budget-exhausted"
        _finish(tele, started)
        return 0

    seen_ids = set(state.get("task_ids", []))
    lines = format_hits(results, seen_ids, remaining)
    if not lines:
        tele["gate"] = "all-deduped"
        _finish(tele, started)
        return 0

    state["lines"] = int(state.get("lines", 0)) + len(lines)
    state["task_ids"] = list(seen_ids)[:400]
    state["last_query_hash"] = query_hash
    _save_state(state_path, state)

    emit_context(event, render_envelope_text(lines))
    tele["gate"] = "injected"
    tele["injected"] = len(lines)
    _finish(tele, started)
    return 0


def _finish(tele: dict[str, Any], started: float) -> None:
    tele["ts"] = _now()
    tele["elapsed_ms"] = round((time.monotonic() - started) * 1000, 1)
    _emit_telemetry(tele)


if __name__ == "__main__":
    try:
        raise SystemExit(run())
    except SystemExit:
        raise
    except Exception:
        # Absolute last-resort fail-open guard.
        raise SystemExit(0)
