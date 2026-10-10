# CLAUDE.md

This file guides Claude Code when working in this repository.

## Commands

- `make test` runs `go test -race ./...`. Tests need `git` on the PATH: the gitrepo and review tests build real repositories with `internal/gitrepo/gitrepotest`.
- `make lint` runs golangci-lint v2 with the strict `.golangci.yaml`, and `make vet` runs `go vet`. Run `golangci-lint fmt -c .golangci.yaml ./...` to fix imports.
- `go run ./cmd/agent-nexus review -forge gitea -repo o/r -pr N -dry-run -transcript` runs one real review without posting it. It needs the LLM and forge env vars listed in README.md.
- `CONTAINER_TOOL=podman make docker-build`. The local docker has no buildx.
- `make helm-lint` lints `charts/agent-nexus` with each `ci/*-values.yaml`. The chart `fail`s on missing required values, so linting it with the defaults is expected to fail.
- A `vX.Y.Z` tag releases the image (`build-image.yaml`) and the OCI chart (`release-chart.yaml`), both at version `X.Y.Z`.

## Architecture

The flow is event → job → handler → agent → forge.

- **`internal/server`:** `POST /v1/events/pr-review`. It authenticates through `internal/auth`, re-reads the PR from the forge, drops stale `head_sha` events, and enqueues the job.
- **`internal/jobs`:** a SQLite store and worker pool.
  - Each job has a `dedupe_key` (one live job per commit) and a `group_key` (a newer commit supersedes older jobs, and cancels them if they are running).
  - `Checkpoint` stores the drafted review and moves the job to `posting`, so crash recovery re-posts instead of re-running the LLM.
- **`internal/review`:** the `pr_review` handler.
  - `Draft` gathers the PR, diff and base-branch `AGENTS.md`, runs the agent, then makes one final schema-constrained call.
  - `Post` is idempotent: it looks for the `<!-- agent-nexus:sha=… -->` marker before posting.
- **`internal/agent`:** the tool-calling loop. It records the transcript and elides old tool output as the conversation nears the context window.
- **`internal/llm`:** a hand-rolled OpenAI-compatible client, with llama-server extras (`reasoning_content`, `chat_template_kwargs`, `/props`).
- **`internal/gitrepo`:** a bare-repo cache plus a hardened git CLI wrapper. Nothing is ever checked out.
- **`internal/tools`:** read-only tools over git objects.
- **`internal/forge`:** Gitea (bot token) and GitHub (App JWT → installation token), as hand-rolled REST clients. `forgetest.Fake` is an in-memory forge for tests.
- **`skills/`:** Agent Skills (`<name>/SKILL.md`, frontmatter `name` and `description` only), embedded via `skills/embed.go`.

## Invariants

- **Model input never becomes a git option.** It goes after `-e`, `--end-of-options` or `--`, or inside a `sha:path` with a path checked by `CleanPath`. Revisions are always SHAs that agent-nexus chose.
- **Forge POSTs are never retried by the HTTP client.** A failed Gitea review can leave a pending review behind. `Post` deletes it and checks for the marker before trying again.
- **Repository guidance comes from the base commit,** never the PR head.
- **Reviews are `COMMENT` unless the repository resolves to verdict mode.** See `resolveMode` in `internal/review/mode.go`: an exact central rule, then `.agent-nexus.yaml` on the base commit, then central patterns, then comment. `verdictEvent` downgrades a verdict that contradicts the findings, and `postAttempts` falls back to a comment if the forge refuses the verdict.

## Style

- Stdlib first: slog, env config, `net/http` ServeMux patterns, `flag`, stdlib `testing`. No testify, cobra or viper.
- Wrap errors as `"<op>: %w"`. Use `errors.New` for constant messages, which perfsprint enforces.
