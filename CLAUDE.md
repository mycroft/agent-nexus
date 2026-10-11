# CLAUDE.md

This file guides Claude Code when working in this repository.

## Commands

- `make test` runs `go test -race ./...`. Tests need `git` on the PATH: the gitrepo and review tests build real repositories with `internal/gitrepo/gitrepotest`.
- `make lint` runs golangci-lint v2 with the strict `.golangci.yaml`, and `make vet` runs `go vet`. Run `golangci-lint fmt -c .golangci.yaml ./...` to fix imports.
- `go run ./cmd/agent-nexus review -forge gitea -repo o/r -pr N -dry-run -transcript` runs one real review without posting it. It needs the LLM and forge env vars listed in README.md.
- `CONTAINER_TOOL=podman make docker-build`. The local docker has no buildx.
- `make css` builds the UI stylesheet with the Tailwind standalone CLI (downloaded to `bin/`); the output is gitignored, and `make build`, `make run` and the Dockerfile build it. Tests do not need it.
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
- **`internal/review` (discussion):** the `pr_discussion` handler (`DiscussionHandler`). `POST /v1/events/pr-comment` queues one with `KeepRunning`, so a newer pass replaces a queued one and a running one finishes.
  - `pendingAsks` finds human replies in threads the bot took part in, and mentions of `@agent-nexus`, that no reply marker covers.
  - Answers follow `skills/pr-discussion`.
  - `renderHistory` feeds the same discussion to later reviews.
- **`internal/agent`:** the tool-calling loop. It records the transcript and elides old tool output as the conversation nears the context window. It also stops investigating when the context's deadline (`JOB_TIMEOUT`) leaves time only for another turn and the final answer, each estimated at the slowest turn so far, so a slow model yields a shorter review, not a timed-out job. `Finish` likewise skips its remaining cache-friendly retries for the schema-constrained fallback (no thinking, always parses) when no time is left for another call. In the last three turns, by the turn limit or the deadline, the loop tells the model how many it has left (`turnsLeftNote`), so it concludes rather than being cut short.
- **`internal/llm`:** a hand-rolled OpenAI-compatible client, with llama-server extras (`reasoning_content`, `chat_template_kwargs`, `/props`).
- **`internal/gitrepo`:** a bare-repo cache plus a hardened git CLI wrapper. Nothing is ever checked out.
- **`internal/tools`:** read-only tools over git objects.
- **`internal/forge`:** Gitea (bot token) and GitHub (App JWT → installation token), as hand-rolled REST clients. `forgetest.Fake` is an in-memory forge for tests.
- **`internal/remote`:** the agent's internet access. `NewHTTPClient` only dials public addresses; `Client` adds size-capped GETs, anonymous OCI registry reads (`RegistryGet`, `ImageLabels`), GitHub releases and pages as text (`FetchPage`).
- **`internal/charts`:** reads Helm chart packages from HTTP (`index.yaml`) and OCI repositories through `remote.Client`, caches them on disk and unpacks them in memory.
- **Remote tools:**
  - `tools.Charts` exposes `chart_files`, `chart_read` and `chart_diff`; the diff uses `gitrepo.DiffBlobs`.
  - `tools.Web` exposes `fetch_url` and `release_notes`.
  - `workspace.chartPolicy`, `pagePolicy` and `imagePolicy` decide what the model may name.
- **`internal/web`:** the read-only jobs UI on `UI_ADDR` (htmx + Tailwind v4, templates and assets embedded). The main page polls `/fragments/live`, which sets `HX-Trigger: jobs-changed` when the running and queued jobs differ from `?seen=`, and the history reloads on that event. A job page polls `/fragments/jobs/{id}/steps?after=<seq>` while the job is unfinished. Tailwind scans `templates/` and `*.go`, so class names in Go maps (`funcs.go`) must be complete literals.
- **`skills/`:** Agent Skills (`<name>/SKILL.md`, frontmatter `name` and `description` only), embedded via `skills/embed.go`.

## Invariants

- **Model input never becomes a git option.** It goes after `-e`, `--end-of-options` or `--`, or inside a `sha:path` with a path checked by `CleanPath`. Revisions are always SHAs that agent-nexus chose.
- **Forge POSTs are never retried by the HTTP client.** A failed Gitea review can leave a pending review behind. `Post` deletes it and checks for the marker before trying again.
- **Repository guidance comes from the base commit,** never the PR head.
- **A review marker names the commit, or for a forced review also the job** (`reviewMarker`). A forced review (a review request, or an `@agent-nexus review` comment, which `requestReview` handles without the LLM) is therefore never mistaken for the commit's earlier review, and is still posted only once.
- **Never trust the triggering comment.** A discussion pass rescans the whole discussion. The bot's own notes never count as asks, which prevents loops, and every answer carries `<!-- agent-nexus:reply-to=<kind>:<id> -->` markers.
- **Model-chosen URLs never carry free text.**
  - A chart repository URL must appear verbatim in the base commit (`Repo.Contains`).
  - A page host or an image name must appear there as a whole token (`Repo.ContainsBounded`); pages take no query string.
  - Otherwise, the host must be on `FETCH_ALLOWED_HOSTS`.
  - Every connection goes through `remote.NewHTTPClient`, which refuses non-public addresses at dial time, redirects included.
  - Never send credentials to these hosts, and never give the agent a fetcher without these guards.
- **The UI has no authentication.** It only reads the store and runs on its own listener, so the deployment puts it behind an authenticating proxy while the API ingress stays reachable with API keys. Keep it read-only, and keep the CSP (`default-src 'self'`, htmx with `allowEval: false`): no inline scripts or styles.
- **The verdict follows the discussion, but only through the bot's own markers.** A review lists its findings (`findingsMarker`); answers record the ones they settle (`resolvesMarker`, counted only on the bot's notes and only for the latest review of the head). `approveIfSettled` approves once all are settled, in verdict mode, after re-reading the head; the approval carries a `sha=<head>:settled=` marker and becomes the latest review, which keeps it idempotent.
- **Reviews are `COMMENT` unless the repository resolves to verdict mode.** See `resolveMode` in `internal/review/mode.go`: an exact central rule, then `.agent-nexus.yaml` on the base commit, then central patterns, then comment. `verdictEvent` downgrades a verdict that contradicts the findings, and `postAttempts` falls back to a comment if the forge refuses the verdict.

## Style

- Stdlib first: slog, env config, `net/http` ServeMux patterns, `flag`, stdlib `testing`. No testify, cobra or viper.
- Wrap errors as `"<op>: %w"`. Use `errors.New` for constant messages, which perfsprint enforces.
