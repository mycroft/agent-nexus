# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

- `make test` runs `go test -race ./...`. Tests need `git` on the PATH: the gitrepo and review tests build real repositories with `internal/gitrepo/gitrepotest`. Run one test with `go test -race ./internal/review -run TestDraftAndPost`.
- `make lint` runs golangci-lint v2 with the strict `.golangci.yaml`, and `make vet` runs `go vet`. Run `golangci-lint fmt -c .golangci.yaml ./...` to fix imports. CI also fails when `go mod tidy -diff` shows a change.
- `MCP_CONFIG=mcp.yaml go run ./cmd/agent-nexus mcp` connects to each MCP server and lists the tools the agent gets, with their size in tokens.
- `go run ./cmd/agent-nexus runner` runs jobs leased from the controller at `CONTROLLER_URL`, with `RUNNER_TOKEN` (one of the controller's `RUNNER_TOKENS`).
- `go run ./cmd/agent-nexus review -forge gitea -repo o/r -pr N -dry-run -transcript` runs one real review without posting it. It needs the LLM and forge env vars listed in README.md. Add `SKILLS_DIR=./skills` to try skill edits without rebuilding.
- `CONTAINER_TOOL=podman make docker-build`. The local docker has no buildx.
- `make css` builds the UI stylesheet with the Tailwind standalone CLI (downloaded to `bin/`); the output is gitignored, and `make build`, `make run` and the Dockerfile build it. Tests do not need it.
- `make helm-lint` lints `charts/agent-nexus` with each `ci/*-values.yaml`. The controller (StatefulSet) and the runner Deployment share their job environment through `agent-nexus.agentEnv` in `_helpers.tpl`: a setting jobs need goes there, a controller-only one (listeners, store, API keys, webhook token) in `statefulset.yaml`. The chart `fail`s on missing required values, so linting it with the defaults is expected to fail.
- A `vX.Y.Z` tag releases the image (`build-image.yaml`) and the OCI chart (`release-chart.yaml`), both at version `X.Y.Z`.

## Architecture

The flow is event → job → handler → agent → forge.

- **`internal/server`:** `POST /v1/events/pr-review`. It authenticates through `internal/auth`, re-reads the PR from the forge, drops stale `head_sha` events, and enqueues the job.
- **`internal/jobs`:** a SQLite store and a worker pool per model.
  - Each job has a `dedupe_key` (one live job per commit) and a `group_key` (a newer commit supersedes older jobs, and cancels them if they are running).
  - `Claim(ClaimSpec)` starts the oldest queued job of the kinds a model serves while the model is under its `Concurrency`, and records the model on the job. A `jobs.Model` without `Handlers` only takes jobs from runners. `Handlers` is retried until the model's LLM answers, so a model that is down stalls only its own jobs.
  - `Checkpoint` stores the drafted review and moves the job to `posting`, so crash recovery re-posts instead of re-running the LLM. A requeued job with a checkpoint is claimed as `posting`, outside the model's limit.
  - Each claim takes a random lease (`Job.Lease`). `AddStep`, `Checkpoint` and `Finish` carry it, and `Finish` releases it even for a superseded job. A model's slots and a group's exclusivity count the jobs whose lease is held, so a superseded job keeps them until its worker lets go. `jobs.Execute`, `Outcome`, `NewRun` and `Queue.Finish` are what a worker needs, wherever it runs.
  - The controller's own leases have no expiry: `Recover` releases them at startup. A runner's lease (`LeaseTTL`) expires unless renewed, and `Reap` then takes the job back.
  - A schema change is a new numbered file in `internal/jobs/migrations/`. Files apply in name order and are recorded in `schema_migrations`.
- **`internal/review`:** the `pr_review` handler.
  - `Draft` gathers the PR, diff and base-branch `AGENTS.md`, runs the agent, then makes one final schema-constrained call.
  - `Post` is idempotent: it looks for the `<!-- agent-nexus:sha=… -->` marker before posting.
  - `internal/diff` parses the diff. `Annotated` shows the model new-side line numbers. A finding becomes an inline comment only on a `Commentable` line; otherwise it goes in the review body.
- **`internal/review` (discussion):** the `pr_discussion` handler (`DiscussionHandler`). `POST /v1/events/pr-comment` queues one with `KeepRunning`, so a newer pass replaces a queued one and a running one finishes. Closed and merged pull requests are discussed too; `prepare` then diffs from `PR.MergeBase`, because the base branch holds the change. Reviews and approvals stay for open pull requests.
  - `pendingAsks` finds human replies in threads the bot took part in, and mentions of `@agent-nexus`, that no reply marker covers.
  - Answers follow `skills/pr-discussion`.
  - `renderHistory` feeds the same discussion to later reviews.
- **`internal/review` (chat):** the `chat_question` handler (`Chat`), for questions asked on Zulip. `POST /v1/events/zulip` receives the bot's outgoing webhook (authenticated by the token in its body, not an API key), queues a pass over the message's conversation with `KeepRunning`, and reacts 👀.
  - `pendingChatAsks` finds the other users' messages mentioning the bot (every one, in a direct conversation) that the bot has not reacted ✅ to.
  - `target` picks the repository or pull request from the first `owner/name[#n]` or forge link in the asks, the earlier messages, the topic, then the channel name. A pull request gets the discussion workspace (`prepare`); a repository gets a branch workspace (`prepareBranch`: the default branch's head, no diff, `tools.Commit`). With neither, the agent still runs, on a bare workspace (`bareWorkspace`): no repository, and the chart and web tools only when `FETCH_ALLOWED_HOSTS` is set. The skill then asks which repository a code question is about.
  - Answers follow `skills/chat`. One answer covers every pending question, then they get ✅.
- **`internal/zulip`:** a hand-rolled Zulip client (`History`, `Send`, `React`) and the outgoing webhook payload. `zuliptest.Fake` is an in-memory Zulip server for tests.
- **`internal/agent`:** the tool-calling loop. It records the transcript and elides old tool output as the conversation nears the context window. It also stops investigating when the context's deadline (`JOB_TIMEOUT`) leaves time only for another turn and the final answer, each estimated at the slowest turn so far, so a slow model yields a shorter review, not a timed-out job. `Finish` likewise skips its remaining cache-friendly retries for the schema-constrained fallback (no thinking, always parses) when no time is left for another call. In the last three turns, by the turn limit or the deadline, the loop tells the model how many it has left (`turnsLeftNote`), so it concludes rather than being cut short.
- **`internal/llm`:** a hand-rolled OpenAI-compatible client, with llama-server extras (`reasoning_content`, `chat_template_kwargs`, `thinking_budget_tokens` from `LLM_THINKING_BUDGET`, `/props`). Only sampling parameters may vary between the calls of a conversation: anything that changes the rendered prompt (tools, template kwargs) costs llama-server its prompt cache.
- **`internal/httpclient`:** the retrying client of the forge, LLM and Zulip clients. It retries network errors, 429 and 5xx on GET, HEAD and DELETE. Only the LLM client retries POSTs (`WithRetryPOST`).
- **`internal/runner`:** the runner API, on the API listener under `/v1/runner/` when `RUNNER_TOKENS` is set (`Server`). A claim long-polls on `Queue.Changed` for a job of the runner's model (`Queue.ClaimFor`: the controller's concurrency and kinds, never the runner's), and hands it with its checkpoint and lease, or releases it if the runner went away. Heartbeats renew the lease (`Queue.Heartbeat`), and `409` tells the runner to stop.
  - `agent-nexus runner` (`runJobs`) builds the same handlers as `serve` for each model it has a `baseURL` for (`runnerModels`), with `Reviewer.Enqueue` going to the controller. `Worker` claims with `Client`, runs `jobs.Execute` with the client as the `jobs.Writer`, renews the lease, stops the job on a `409` or after `unreachableAfter` without renewal (before the controller reaps it), and releases its jobs at shutdown.
- **`internal/models`:** the LLMs jobs run on. `MODELS_CONFIG` names a YAML file (`LoadConfig`, like `internal/mcp`); without it, `LLM_*` configure one model named `default`. `main` builds one handler set per model through `Reviewer.WithModel`. The UI and `agent_nexus_model_jobs`/`_concurrency` show each model's slots in use (`Store.ModelUsage`); job and LLM metrics carry a `model` label (`llm.WithName`).
- **`internal/config`:** every setting is an env var read by `config.Load`. A new one usually also goes in README's table, the chart's `values.yaml` and `templates/statefulset.yaml`.
- **`internal/gitrepo`:** a bare-repo cache plus a hardened git CLI wrapper. Nothing is ever checked out.
- **`internal/tools`:** read-only tools over git objects: `Commit` at a commit, `Repo` adds a pull request's `get_file_diff`.
- **`internal/forge`:** Gitea (bot token) and GitHub (App JWT → installation token), as hand-rolled REST clients. `forgetest.Fake` is an in-memory forge for tests.
- **`internal/remote`:** the agent's internet access. `NewHTTPClient` only dials public addresses; `Client` adds size-capped GETs, anonymous OCI registry reads (`RegistryGet`, `ImageLabels`), GitHub releases and pages as text (`FetchPage`).
- **`internal/mcp`:** the tools of MCP servers, through the official go-sdk, which speaks both protocol eras: `server/discover` (2026-07-28), and `initialize` for servers with sessions.
  - `MCP_CONFIG` names a YAML file (`LoadConfig`). Each server has a required tool allowlist, the jobs it serves (`chat` by default), and a `load` mode (`lazy` by default).
  - `Reviewer.openMCP` (`internal/review/mcp.go`) sets up a job's servers. It opens the eager ones in parallel, and `tools.MCP` offers their tools as `<server>_<tool>`. A server that is down is left out.
  - Lazy servers are only named in the prompt. `tools.MCPLazy` gives `mcp_tools`, which opens a server on first use (`mcp.Lazy`) and lists its tools as text, and `mcp_call`. Either way the tool list never changes within a conversation, so llama-server keeps its prompt cache.
  - `mcptest.Fake` is the SDK's own server. The SDK serves 2026-07-28 only when stateless, so `LegacyOnly` (sessions) is how the older era is tested.
- **`internal/charts`:** reads Helm chart packages from HTTP (`index.yaml`) and OCI repositories through `remote.Client`, caches them on disk and unpacks them in memory.
- **Remote tools:**
  - `tools.Charts` exposes `chart_files`, `chart_read` and `chart_diff`; the diff uses `gitrepo.DiffBlobs`.
  - `tools.Web` exposes `fetch_url` and `release_notes`.
  - `tools.Repos` exposes `repo_log` and `repo_read`, over other repositories of the forge. They go through `gitrepo.Cache.FetchRefs` and the forge's `GitRemote` credentials, not through `internal/remote`; `workspace.openRepo` is their policy.
  - `workspace.chartPolicy`, `pagePolicy` and `imagePolicy` decide what the model may name.
- **`internal/web`:** the read-only jobs UI on `UI_ADDR` (htmx + Tailwind v4, templates and assets embedded). The main page polls `/fragments/live`, which sets `HX-Trigger: jobs-changed` when the running and queued jobs differ from `?seen=`, and the history reloads on that event. A job page polls `/fragments/jobs/{id}/steps?after=<seq>` while the job is unfinished. Tailwind scans `templates/` and `*.go`, so class names in Go maps (`funcs.go`) must be complete literals.
- **`skills/`:** Agent Skills (`<name>/SKILL.md`, frontmatter `name` and `description` only), embedded via `skills/embed.go`.

## Invariants

- **The runner API never carries credentials.** A runner reads its own forge, Zulip, MCP and LLM credentials from its environment; the controller only sends jobs, leases and checkpoints.
- **Model input never becomes a git option.** It goes after `-e`, `--end-of-options` or `--`, or inside a `sha:path` with a path checked by `CleanPath`. Revisions are always SHAs that agent-nexus chose.
- **Forge POSTs are never retried by the HTTP client.** A failed Gitea review can leave a pending review behind. `Post` deletes it and checks for the marker before trying again.
- **A worker writes to a job only through its claim's lease.** Never add a store write on a started job that does not check the lease: a worker that lost the job (superseded and released, or reaped) must not touch the new attempt.
- **Repository guidance comes from the base commit,** never the PR head.
- **A review marker names the commit, or for a forced review also the job** (`reviewMarker`). A forced review (a review request, or an `@agent-nexus review` comment, which `requestReview` handles without the LLM) is therefore never mistaken for the commit's earlier review, and is still posted only once.
- **Chat answers never write to a forge,** and only name repositories of `ALLOWED_OWNERS`. Only the bot's own ✅ reaction marks a Zulip question answered.
- **Never trust the triggering comment.** A discussion pass rescans the whole discussion. The bot's own notes never count as asks, which prevents loops, and every answer carries `<!-- agent-nexus:reply-to=<kind>:<id> -->` markers.
- **Model-chosen URLs never carry free text.**
  - A chart repository URL must appear verbatim in the base commit (`Repo.Contains`).
  - A page host or an image name must appear there as a whole token (`Repo.ContainsBounded`); pages take no query string.
  - Otherwise, the host must be on `FETCH_ALLOWED_HOSTS`. A bare workspace has no base commit, so only these hosts count.
  - Another repository of the forge must be the reviewed one, or appear in the base commit as `<forge host>/<owner>/<name>` (`openRepo`). It is read with the forge's credentials only: never add credentials for other hosts to agent-nexus. Those belong in a proxy or an MCP server outside it.
  - Every connection goes through `remote.NewHTTPClient`, which refuses non-public addresses at dial time, redirects included.
  - Never send credentials to these hosts, and never give the agent a fetcher without these guards.
- **MCP servers are chosen by the operator, never by the model.**
  - Their URLs come from `MCP_CONFIG`, so they may be private, and their client follows no redirect. A lazy server is named by the model only among the job's servers.
  - Only the allowlisted tools are offered, and only to the jobs a server lists; `Session.Call` refuses any other. Chat is the default because PR text is untrusted.
  - The one credential agent-nexus holds for a server is the token reaching it. The backend's credentials stay in the server.
- **The UI has no authentication.** It only reads the store and runs on its own listener, so the deployment puts it behind an authenticating proxy while the API ingress stays reachable with API keys. Keep it read-only, and keep the CSP (`default-src 'self'`, htmx with `allowEval: false`): no inline scripts or styles.
- **The verdict follows the discussion, but only through the bot's own markers.** A review lists its findings (`findingsMarker`); answers record the ones they settle (`resolvesMarker`, counted only on the bot's notes and only for the latest review of the head). `approveIfSettled` approves once all are settled, in verdict mode, after re-reading the head; the approval carries a `sha=<head>:settled=` marker and becomes the latest review, which keeps it idempotent.
- **Reviews are `COMMENT` unless the repository resolves to verdict mode.** See `resolveMode` in `internal/review/mode.go`: an exact central rule, then `.agent-nexus.yaml` on the base commit, then central patterns, then comment. `verdictEvent` downgrades a verdict that contradicts the findings, and `postAttempts` falls back to a comment if the forge refuses the verdict.

## Testing

- Tests run against fakes, never a live service:
  - `forgetest.Fake` for forges;
  - `zuliptest.Fake` for Zulip;
  - `gitrepotest.New` for a repository with a base and a PR head;
  - `chartstest.NewRepo` for a chart repository;
  - `mcptest.New` for an MCP server.
- `internal/review` tests build on `newFixture` and `fakeLLM`. `fakeLLM` replays canned assistant messages in order and fails on an extra call. The last message is the final JSON (`fixture.script`).
- `remote.NewHTTPClient` refuses loopback, so tests pass `srv.Client()` to `remote.New` to reach an `httptest` server.

## Style

- Stdlib first: slog, env config, `net/http` ServeMux patterns, `flag`, stdlib `testing`. No testify, cobra or viper.
- Clients are hand-rolled, except MCP, which goes through `github.com/modelcontextprotocol/go-sdk` because the protocol has two eras.
- Wrap errors as `"<op>: %w"`. Use `errors.New` for constant messages, which perfsprint enforces.
