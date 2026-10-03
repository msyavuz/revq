<img src="internal/web/static/icon.svg" width="48" alt="">

# revq

A self-hosted review queue for maintainers. revq watches the pull requests waiting on
your review, has Claude Code draft a review for each, and shows what needs you on a
kanban board. Nothing is posted to GitHub until you approve it, unless you turn that on.

## Run it

On a home server with Docker:

    cp .env.example .env       # fill in the tokens, see below
    docker compose up -d --build

Open `http://<server>:8080`, go to Settings, and add a repository. Data is one SQLite
file in the `revq-data` volume. To update: `git pull && docker compose up -d --build`.

Locally, using your existing `gh` and `claude` logins:

    go run ./cmd/revq          # http://127.0.0.1:8080

## Environment

| Variable | Default | |
|---|---|---|
| `GITHUB_TOKEN` | `gh auth token` | Reviews are posted as this token's owner. Read access is enough to draft; posting reviews or labels needs pull request write access. |
| `CLAUDE_CODE_OAUTH_TOKEN` | local `claude` login | From `claude setup-token`. Uses your Claude subscription. |
| `ANTHROPIC_API_KEY` | unset | Alternative to the OAuth token, billed per use. |
| `REVQ_PASSWORD` | unset | Basic-auth password for the UI (any username). Set it if the port is reachable by anyone else. |
| `REVQ_ADDR` | `127.0.0.1:8080` | Listen address (`:8080` in the Docker image). |
| `REVQ_DB` | `revq.db` | SQLite file. |
| `REVQ_CLAUDE_BIN` | `claude` | Path to the Claude Code CLI. |

## How it works

**What it tracks.** Per repository, either the PRs where your review is requested plus
the ones you've already reviewed (the default), or every open PR. It polls GitHub, so it
works behind NAT with no public URL.

**The board.** Inbox, Needs you, Waiting on author, Ready to merge, Done. Cards place
themselves from review requests, CI and review state. Drag a card to pin it somewhere
until the author pushes again. The bar on each card is the size of the change.

**The agent.** "Draft review" on a card runs a cheap triage (summary, risk, questions
only you can answer, which files matter) and then a review. You edit the draft, drop
findings you disagree with, and post it from the PR page.

**Autonomy.** Off by default. Three switches, global with a per-repo override:

- Auto review: draft a review for every PR waiting on you, once per commit.
- Auto post: publish drafts without your approval. Always as a plain comment; approving
  or requesting changes is only ever done by you.
- Auto label: apply the labels the agent suggests, limited to labels the repo already has.

**Notifications.** A Slack-compatible webhook and/or a Telegram bot get a message when a
PR lands in Needs you.

## Token cost

- Syncing uses the GitHub API only. No model is involved.
- Triage sees the file list and a budgeted excerpt of the diff, with thinking off. A few
  cents even for a 25k-line PR.
- Review skips lockfiles, generated and vendored files, packs the rest focus-files-first
  into a capped number of chunks, and lists anything it left out on the draft.
- The agent is `claude -p` with no tools, no MCP servers, no settings and its own system
  prompt, so the fixed overhead is about 1k tokens per call. With no tools, text inside a
  PR can't make it do anything.
- A daily budget pauses the queue. A pushed commit is not re-reviewed while a draft is
  still waiting for you.

Models, budgets, diff sizes, extra ignore paths and your project's review guidelines are
on the Settings page.

## Limits

- GitHub only, one user, one token.
- The reviewer sees the diff, not a checkout of the repository.
- No automated tests yet.

## License

MIT. Bundled fonts (Schibsted Grotesk, Commit Mono) are under the SIL Open Font License;
their license texts are in `internal/web/static/fonts`. htmx is 0BSD.
