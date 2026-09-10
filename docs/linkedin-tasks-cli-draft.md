# LinkedIn draft — tasks-cli

I used to be a big Jira user. For my own work, it has now quietly been replaced by something much smaller.

I first built an MCP server so Claude, Codex and Copilot could share one backlog. Then the local architecture changed twice: the agents moved to a Go command-line tool, and the last GUI integration moved from the old HTTP/MCP route to invoking that same CLI.

The data stayed the same. Markdown files are the source of truth, the search index is disposable, and every command returns JSON an agent or a human can inspect. The interface changed. A local agent already has a shell; a GUI can call a thin adapter that shells out to the same binary. There is no second task implementation to drift.

That was the useful lesson. Shared tooling does not mean every team needs the same server. It means we need one stable contract: commands, JSON, exit codes, versioning, locking and clear ownership. For local deterministic work, a binary can be the simplest shared interface. For remote or multi-tenant work, an API or MCP boundary still earns its keep.

Same backlog. Fewer moving parts. Clearer trade-offs.

Repo: https://github.com/GanizaniSitara/tasks-cli
