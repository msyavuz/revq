# Contributing to revq

Thanks for wanting to help. revq is small on purpose, so the most useful thing to read
first is what it is not trying to be.

## Scope

revq is a review queue for one maintainer: it shows the pull requests waiting on you,
has an agent draft inline comments, and lets you approve, edit or drop them.

It is not a GitHub replacement. These are out of scope, and a PR adding them will
likely be declined:

- a full diff viewer, comment threads or replies
- merge buttons, CI details, editing PR metadata
- multi-user accounts, teams or permissions
- support for forges other than GitHub, for now

A change is a good fit if it helps you decide what to do with a drafted review, or gets
you through the queue faster. If you're unsure, open an issue before writing code.

## Running it locally

You need Go 1.25 or newer, the `claude` CLI on your `PATH`, and either `GITHUB_TOKEN`
set or a signed-in `gh`.

    go run ./cmd/revq          # http://127.0.0.1:8080, sign in as admin / admin

It stores everything in `revq.db` in the current directory. Delete that file to start
over. Agent runs cost real money on your own Claude account; they only happen when you
press "Draft review" unless you turn automation on.

## Before you open a pull request

    gofmt -l .                 # prints nothing when formatted
    go vet ./...
    go test ./...
    go build ./cmd/revq

CI runs the same checks.

- Keep a PR to one change. Small PRs get reviewed faster.
- Add a test when you fix a bug or change logic that can be tested without GitHub or
  the agent. Templates and CSS don't need tests; say how you checked them.
- For anything visible, include a before and after screenshot. Use made-up data, not
  real people's pull requests.
- Don't add dependencies without a good reason. revq has two: SQLite and a Markdown
  renderer.
- The guide lives in `docs/guide.md`. Update it when behaviour a user sees changes.
  `go run ./docs/build` builds the site into `docs/_site` if you want to look at it.

## Layout

| Path | What's there |
|---|---|
| `cmd/revq` | Entry point and commands |
| `internal/store` | SQLite storage, settings, account |
| `internal/github` | The GitHub API calls revq makes |
| `internal/agent` | Running Claude Code for one structured call |
| `internal/pipeline` | Sync, review drafting, where a card sits on the board |
| `internal/web` | Pages, templates, CSS and the little JavaScript there is |
| `internal/update` | Self-update |
| `docs` | The guide and the site generator |

## Reporting a security problem

Please don't open a public issue. Use "Report a vulnerability" under the repository's
Security tab, which reaches the maintainer privately.

## License

By contributing you agree that your work is released under the MIT license in `LICENSE`.
