# LinkedIn draft — tasks-cli

I used to be a big Jira user. For my own work, it has now quietly been replaced by something much smaller.

I first built an MCP server so Claude, Codex and Copilot could all work from the same backlog. Then I replaced the server with a command-line tool — and it got better.

The tasks are still ordinary Markdown files. Bleve provides a disposable search index. But the integration is now one Go binary that emits JSON.

The practical difference is quite large:

- there is no server process, port or session to keep alive;
- every coding agent can already use a shell, so there is no agent-specific MCP setup;
- the input and output are easy to inspect; and
- the whole thing tests like a normal program, in about a second.

This isn't an argument against MCP. It earns its keep for remote services and genuinely stateful integrations. For a local corpus of files, I found that a running server was ceremony around something a command already did.

The move also exposed a more interesting problem.

Bleve's standard match query treats multiple words as alternatives. So a search for `scraper quantumfoobar` returned a page of plausible-looking scraper tickets even though `quantumfoobar` appeared nowhere. The old search engine had required both terms, and I had carried that assumption across without testing it.

The CLI now searches in two explicit passes. First, every term must appear somewhere across the task ID, title, tags or body. If that finds nothing, it can fall back to any-term matching — but the JSON says which pass produced the answer.

That distinction matters more than the search engine. Retrieval systems are very good at returning the nearest thing. A useful one also needs to say, clearly, "nothing here".

There is a small prompt hook too. It injects one-line pointers to relevant tickets, then lets the agent fetch the full task only when it looks useful. That has stopped quite a few confident attempts to rebuild work that was already written down.

Nothing revolutionary. Just Markdown as the source of truth, a small binary as the interface, and fewer moving parts between an agent and the work.

The repo is public. Happy to share it if useful.
