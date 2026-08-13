# LinkedIn draft — tasks-cli

I used to be a big Jira user. For my own work, it has now quietly been replaced by something much smaller.

I first built an MCP server so Claude, Codex and Copilot could share one backlog. Then I replaced the server with a command-line tool — and it got better.

Every agent can already use a shell. So there is no server, port or agent-specific MCP setup to keep alive.

The tasks remain ordinary Markdown files. The search index is disposable. Every command returns inspectable JSON.

This isn't anti-MCP. It makes sense for remote services. For a local collection of files, though, the command line was enough.

Markdown as the source of truth. One small binary for every coding agent. Fewer moving parts.

Repo: https://github.com/GanizaniSitara/tasks-cli
