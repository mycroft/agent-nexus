# agent-nexus

agent-nexus listens for events, runs LLM agent jobs against a llama.cpp
`llama-server`, and posts the results back. Version 1 does pull request
reviews on Gitea and GitHub.

```
workflow ──POST /v1/events/pr-review──▶ API (API key, owner allowlist, head check)
                                          │ enqueue (dedupe per commit, supersede older commits)
                                          ▼
                              SQLite job store + worker pool
                                          ▼
          review: fetch PR → git fetch (bare cache) → diff → agent loop
                  (pr-review skill + read-only git tools) → JSON review
                                          ▼
                          PR review with inline comments
```

- **Fire and forget.** The workflow posts an event and exits within seconds.
  agent-nexus queues the job and posts the review as its bot.
- **Only identifiers are trusted.** The workflow sends `server_url`, `repo`,
  `pr` and `head_sha`. agent-nexus reads the pull request from the forge
  itself, and drops events whose `head_sha` is no longer the head.
- **Read-only, sandboxed tools.** The model can call `read_file`,
  `list_dir`, `grep`, `git_log` and `get_file_diff`. They read git objects
  at the head commit and never touch a checkout. Arguments the model
  supplies cannot become git options.
- **Repository guidance.** `AGENTS.md` (or `CLAUDE.md`) is read from the
  *base* branch, so a pull request cannot rewrite its own review rules.
- **Idempotent posting.** Each review carries a hidden marker with the
  commit SHA. A job that is retried, or recovered after a crash, never
  posts twice.
- **Answers replies.** agent-nexus answers replies in its own review threads,
  and comments that mention `@agent-nexus`. Its later reviews take the
  discussion into account (see [Discussion](#discussion)).
- **Answers questions on Zulip.** Mention the bot in a channel, or send it
  a direct message, about a repository, a PR, or anything else (see
  [Zulip](#zulip)).
- **Comment or verdict, per repository.** By default a review is a plain
  comment. Repositories in verdict mode get an approval, a comment or a
  change request instead (see [Review modes](#review-modes)).
- **Live state through MCP.** The agent can call the tools of MCP
  servers, such as Kubernetes or Grafana, limited to the tools and jobs
  you list. It loads a server's tools when it needs them (see
  [MCP servers](#mcp-servers)).
- **A jobs UI.** It shows what runs, the job history, and every step the
  agent took (see [Jobs UI](#jobs-ui)).

The review instructions live in [`skills/pr-review/SKILL.md`](skills/pr-review/SKILL.md),
which uses the Agent Skills format and is embedded in the binary.

## Commands

```sh
agent-nexus serve      # HTTP API + workers
agent-nexus runner     # run jobs leased from a controller (see Runners)
agent-nexus review -forge gitea -repo mycroft/k8s-home -pr 42 -dry-run -transcript
agent-nexus mcp        # the tools each MCP server gives the agent
agent-nexus version
```

`review` runs one review synchronously and prints it. With `-dry-run` it
does not post, which makes it the quickest way to iterate on the skill.
Point `SKILLS_DIR=./skills` at the working tree to skip rebuilding.

## Configuration

Everything is configured through environment variables.

| Variable | Default | |
|---|---|---|
| `LLM_BASE_URL` | required without `MODELS_CONFIG` | OpenAI-compatible root, e.g. `https://llm-api.iop.cx/v1` |
| `LLM_API_KEY` | | Bearer token for the LLM server |
| `LLM_MODEL` | required without `MODELS_CONFIG` | Model name sent with each request |
| `LLM_CONTEXT_TOKENS` | from `/props` | Context window; set it to avoid the lookup |
| `LLM_THINKING_BUDGET` | server default | Most reasoning tokens per request (llama-server `thinking_budget_tokens`); `0` keeps the server's default |
| `LLM_CONCURRENCY` | `1` | Parallel jobs; match llama-server `--parallel` |
| `LLM_REQUEST_TIMEOUT` | `10m` | One completion, including time queued in llama-server |
| `MODELS_CONFIG` | | YAML file of several models, instead of the `LLM_*` variables (see [Models](#models)) |
| `AGENT_MAX_TURNS` | `30` | Tool-loop turns per review |
| `JOB_TIMEOUT` | `30m` | One job end to end; the investigation stops early enough to write the review before it |
| `API_KEYS` | required for `serve` | `name=key,name=key`, each key ≥ 32 characters |
| `RUNNER_TOKENS` | | `name=token,…`, each token ≥ 32 characters: the runners `serve` hands jobs to (see [Runners](#runners)) |
| `CONTROLLER_URL`, `RUNNER_TOKEN` | required for `runner` | The controller's API root, and this runner's token |
| `RUNNER_NAME` | host name | This runner instance, as jobs and the UI name it: `<token name>/<RUNNER_NAME>` |
| `ALLOWED_OWNERS` | required for `serve` | Comma-separated repository owners |
| `EVENTS_PER_MINUTE` | `30` | Rate limit per API key |
| `REVIEW_MODES` | | Central review mode rules, `pattern=mode,…` (see below) |
| `GITEA_URL`, `GITEA_USER`, `GITEA_TOKEN` | | Gitea forge (bot account) |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` | | GitHub forge (App, PEM key) |
| `GITHUB_API_URL`, `GITHUB_SERVER_URL` | github.com | For GitHub Enterprise |
| `ZULIP_URL`, `ZULIP_BOT_EMAIL`, `ZULIP_BOT_API_KEY`, `ZULIP_WEBHOOK_TOKEN` | | Zulip outgoing webhook bot: server root, then the `email`, `key` and `token` of its zuliprc. Only `serve` needs the token |
| `LISTEN_ADDR` | `:8080` | API |
| `METRICS_ADDR` | `:9090` | `/metrics`; keep it out of the Ingress |
| `UI_ADDR` | `:8081` | Jobs UI, unauthenticated: expose it only behind an authenticating proxy |
| `DB_PATH` | `agent-nexus.db` | SQLite database |
| `REPO_CACHE_DIR` | `repos` | Bare repository cache |
| `CHART_CACHE_DIR` | `charts` | Helm chart packages and indexes read by the agent |
| `FETCH_ALLOWED_HOSTS` | | Comma-separated hosts the agent may read from (chart repositories, pages, image registries) besides those the reviewed repository references |
| `TRANSCRIPT_RETENTION` | `720h` | How long agent transcripts are kept |
| `SKILLS_DIR` | embedded | Load skills from disk instead |
| `MCP_CONFIG` | | YAML file of the MCP servers the agent may call (see [MCP servers](#mcp-servers)) |
| `LOG_LEVEL` | `info` | |

At least one forge must be configured.

## Models

The `LLM_*` variables configure one model, named `default`. To run jobs
on several LLMs, `MODELS_CONFIG` names a YAML file listing them instead;
setting both is an error. The Helm chart renders `models` into it, each
`apiKeyKey` naming the key of `existingSecret` holding that model's API
key; leave `llm.baseURL` and `llm.model` empty then.

```yaml
models:
  - name: qwen            # lowercase letters and digits
    baseURL: https://llm-api.iop.cx/llama.cpp/v1
    model: Swift Uncensored Qwen 3.8   # sent with each request, shown in reviews
    apiKeyEnv: LLM_API_KEY_QWEN        # environment variable holding its API key
    concurrency: 1        # jobs running on it at once; match llama-server --parallel
    jobs: [review, discussion, chat]   # the jobs it serves; default all
    contextTokens: 0      # 0 asks llama-server's /props
    thinkingBudget: 0     # 0 keeps the server's default
    requestTimeout: 10m
  - name: small
    baseURL: http://llm-small.lan:8080/v1
    model: Qwen3-8B
    concurrency: 2
    jobs: [chat]
```

A queued job runs on the first model with a free slot among those
serving it, so the second model above takes chat questions while the
first reviews. A job claimed by a model runs to the end on it; the jobs
page names it. Every job enabled (chat needs Zulip) must be served by some
model.

agent-nexus asks each model for its context window before giving it a
job. A model whose server does not answer takes no job and is asked
again, backing off up to five minutes; its jobs go to the other models
serving them, or wait. `agent-nexus review -model <name>` reviews with a
given model; by default, with the first one serving reviews.

## Runners

By default, `serve` runs every job itself. With `RUNNER_TOKENS`, other
processes can run jobs too: `agent-nexus runner`, on another machine next
to its LLM, or in more pods. A runner only dials the controller, over the
same API listener (see [API](#api)); the controller never dials it.

```sh
CONTROLLER_URL=https://agent-nexus.example.com RUNNER_TOKEN=… \
MODELS_CONFIG=models.yaml GITEA_URL=… GITEA_USER=… GITEA_TOKEN=… \
ALLOWED_OWNERS=mycroft agent-nexus runner
```

- **What runs where.** The controller keeps the job store, the events,
  the UI and the models' limits: a model's `concurrency` in its own
  `MODELS_CONFIG` counts the jobs of every runner and its own. A runner
  serves each model of its `MODELS_CONFIG` that has a `baseURL`, with up
  to that model's `concurrency` jobs of its own, of the jobs the
  controller's configuration gives the model. To leave a model to the
  runners, give it `local: false`, or no `baseURL`, on the controller.
- **A runner runs the whole job,** with its own configuration: the
  forges, Zulip (without `ZULIP_WEBHOOK_TOKEN`), MCP servers, caches,
  `ALLOWED_OWNERS`, `REVIEW_MODES` and `FETCH_ALLOWED_HOSTS`. Keep these
  in line with the controller's, or the answer depends on who ran the job.
  The runner API never carries credentials: each runner has its own.
- **MCP servers** must be reachable from the runner: an in-cluster URL is
  not, from another machine. A job leaves out a server it cannot reach.
- **Caches.** Each runner keeps its own `REPO_CACHE_DIR` and
  `CHART_CACHE_DIR`; never share one between processes.
- **Leases.** A runner renews its job's lease every 15 seconds. When the
  controller supersedes the job, the next renewal stops it. A runner that
  cannot reach the controller for 90 seconds stops the job and gives it
  back; one that disappears loses it after 2 minutes, and the job is
  queued again, for at most 3 attempts. At shutdown, a runner gives its
  jobs back at once.
- **Metrics.** A runner serves its own LLM, tool and MCP metrics, and
  `/healthz`, on `METRICS_ADDR`. Job metrics stay on the controller.

The Helm chart runs runner pods next to the controller with
`runners.enabled` and `runners.replicas`. They share the controller's
configuration (models, forges, Zulip, MCP servers), dial it through its
Service, and keep their caches in an emptyDir. Their
`app.kubernetes.io/name` is `<name>-runner`, so they never join the
controller's Services; a NetworkPolicy must let them reach its `http`
port. With `runners.enabled` and no replicas, the controller only takes
runners from elsewhere: give each its own token in `runner-tokens`.

```yaml
models:
  - name: big
    baseURL: http://llm.llm.svc:8080/v1
    model: Qwen3-32B
    concurrency: 2     # across the controller and both runners
    local: false       # only the runners run it
runners:
  enabled: true
  replicas: 2
```

## Review modes

Each repository is reviewed in one of two modes:

- **`comment`** (the default): the review is a plain comment with a
  summary and findings, and no verdict.
- **`verdict`**: the review is submitted as an approval, a comment or a
  change request, following its verdict. A verdict that contradicts the
  findings is downgraded to a comment, with a note explaining why:
  - approving requires no blocking or important finding;
  - requesting changes requires a blocking finding.

If the forge refuses the verdict, for example because the bot may only
comment, the review is posted as a comment.

The verdict also follows the discussion. Once every finding of the bot's
latest review of the head is settled in replies, it approves the pull
request without a new review (see [Discussion](#discussion)).

The mode comes from the first of these that applies:

1. **A central rule naming the repository exactly.**
2. **`review.mode` in the repository's `.agent-nexus.yaml`**, read from
   the base branch, so a pull request cannot change its own mode.
3. **A central rule with a pattern.** Patterns use `path.Match`, e.g.
   `mycroft/*`.
4. **`comment`.**

Central rules are an ordered list: `REVIEW_MODES`, or `review.modes` in
the Helm chart.

```yaml
# Helm values
review:
  modes:
    - repo: mycroft/k8s-home   # always verdict, whatever its file says
      mode: verdict
    - repo: "mycroft/*"        # others: comment, unless their file says otherwise
      mode: comment
```

```yaml
# .agent-nexus.yaml, at the root of a repository
review:
  mode: verdict
```

When `.agent-nexus.yaml` is invalid, agent-nexus ignores it and says so
in the review.

On Gitea, an approval counts toward branch protection only when the
reviewer has write access or is in the approvals allowlist. A read-only
bot's approvals are shown but are not "official".

## Reading charts, release notes and docs

### Helm charts

Most version-bump PRs change one line, such as
`prometheus-community/kube-prometheus-stack: 92.3.0`. The agent can read
the chart versions themselves:

- `chart_files` lists a version's files;
- `chart_read` reads one file;
- `chart_diff` diffs two versions: which files changed, then their diffs,
  `Chart.yaml` and `values.yaml` first.

A review uses them to check what the repository sets against keys the new
version renamed or removed, changed defaults and new required values.

They read classic Helm repositories (`https://…`, through `index.yaml`,
with the package digest checked) and OCI registries (`oci://…`, with an
anonymous registry token). Subcharts shipped as archives are unpacked
under `charts/<name>/`.

The model chooses the repository URL, so a chart URL is accepted only if:

- **it appears in the reviewed repository's base branch**, which a pull
  request cannot change. The model can only name repositories already in
  use, and cannot carry data out in a URL;
- **or its host is in `FETCH_ALLOWED_HOSTS`**.

### Release notes and pages

- **`release_notes(image, from_version, to_version)`** reads the GitHub
  releases of a container image's project, after `from` up to `to`.
  - It finds the project from the image's
    `org.opencontainers.image.source` label, or from its name for ghcr.io.
  - Versions compare by their numbers, so image tag `v2.5` matches release
    `v2.5.0`.
  - Pre-releases count only when `to` is one.
  - It uses the GitHub API anonymously (60 requests an hour).
- **`fetch_url(url)`** reads a page as text: HTML is converted, markdown and
  plain text are kept. GitHub file links are read from their raw content.
  Pages are cached for 15 minutes, so reading one in parts fetches it once.

What the model may ask for is limited the same way:
- **Images:** the image name must appear in the base branch, matched as a
  whole name: `haals/yopass` doesn't ride on `jhaals/yopass`. Otherwise,
  its registry must be on `FETCH_ALLOWED_HOSTS`.
- **Pages:** the page's host must appear in the base branch, matched as a
  whole hostname: `community.github.io` doesn't ride on
  `prometheus-community.github.io`. Otherwise, it must be on
  `FETCH_ALLOWED_HOSTS`. Query strings are refused.

### Other repositories of the forge

Self-built images and charts usually have no public release notes. Their
source is another repository on the same forge, often private.

- **`repo_log(repo, from, to)`** lists that repository's commits between
  two tags or branches, with their messages and a diffstat. `0.1.6` also
  matches the tag `v0.1.6`.
- **`repo_read(repo, ref, path)`** reads one of its files at a tag or
  branch.

They fetch through the git cache with the forge's own credentials, the
ones agent-nexus already uses to review, so no new token is involved:

- **Gitea** uses the bot's token. The bot reads only what its account
  may read, so add it as a read collaborator where needed.
- **GitHub** uses the App's installation token, which only covers the
  repositories where the App is installed.

The repository must be the one under review, or appear in its base
branch as `<forge host>/<owner>/<name>`, for example a chart's `sources`
or a clone URL. The name is matched as a whole token.

### Network safety

Every connection, redirects included, must reach a public address, so
neither a URL nor a redirect can reach the cluster, the local network or
the host. No credentials are sent, and proxy settings are ignored.

Chart packages are cached under `CHART_CACHE_DIR` for good, since a published
version doesn't change; indexes are refreshed hourly. Sizes are capped:
32 MB per index, 16 MB per package, and 64 MB or 5000 files once
unpacked. Packages are read in memory, never unpacked to disk.

## Discussion

agent-nexus answers:

- **Replies in an inline thread it took part in.** It answers in the
  thread.
- **Comments that mention `@agent-nexus`.** This covers PR comments,
  review bodies and inline comments. It answers with a PR comment that
  quotes the message.

This works on closed and merged PRs too, for questions about a change
after it landed. The diff then comes from the merge base the forge
recorded, since the base branch now holds the PR. Reviews, review
requests and approvals remain for open PRs: `@agent-nexus review` on a
closed one gets a reply saying so.

It doesn't use the comment that triggered the event. Each pass reads the
whole discussion and answers what is still waiting, so a missed event is
caught up by the next one:

- One answer covers every pending message of a thread.
- Each answer carries a hidden `<!-- agent-nexus:reply-to=… -->` marker,
  so nothing is answered twice.
- At most three conversations are answered per pass.

Answers follow [`skills/pr-discussion/SKILL.md`](skills/pr-discussion/SKILL.md).
The agent checks the reply against the code with the same read-only tools,
then either withdraws the finding or keeps it, with evidence.

A new review also reads the earlier discussion: its previous reviews,
the threads it took part in, and the PR comments. It doesn't repeat a
finding that was answered convincingly or fixed since.

**Settling findings.** Each review lists its findings in a hidden
`<!-- agent-nexus:findings=… -->` marker. An answer says which findings of
the latest review of the head it settles:

- the bot withdrew it;
- the head commit fixes it;
- its author deferred it, and the bot agrees it needn't block the PR.

It records them in a hidden `<!-- agent-nexus:resolves=… -->` marker; only
the bot's own markers count.

In a `verdict` repository, once every finding is settled, agent-nexus
submits an approval: a "verdict update" listing the findings. No new
review runs. It then counts as the latest review, so it is submitted only
once. A new commit, or a requested review, starts over with that review's
findings.

**Asking for another review.** A commit is normally reviewed once. To get
a new review of the current head, do either of these:

- request a review from `agent-nexus` in the PR's reviewers (Gitea);
- comment `@agent-nexus review`, `@agent-nexus review again` or
  `@agent-nexus re-review`.

Either way, the review runs even if that commit was already reviewed. The
comment is answered with the queued job's number, without asking the
model.

**Token scopes.** Reading PR comments needs the Gitea token's `read:issue`
scope; without it, agent-nexus sees review threads only.
- Mentions in PR comments, and those comments in the earlier discussion,
  need `read:issue`.
- Answering mentions with a PR comment needs `write:issue`. Without it,
  agent-nexus answers with a comment-only review instead.

## Zulip

With a Zulip bot configured, agent-nexus answers questions about the
repositories and PRs it can read on its forges, and questions about
nothing in particular. In a channel, mention the
bot (`@**agent-nexus**`); in a direct message, just ask. It answers in the
same topic or conversation, mentioning the askers in a channel.

**What it reads.** A pass works out what the conversation is about from,
in order:

1. the questions it answers, newest first;
2. the other users' earlier messages;
3. the topic name;
4. the channel name.

The first reference found wins:

- `owner/name#12`, or a link to a PR, is a PR. agent-nexus reads it like
  a PR discussion: the diff, the head commit, the forge discussion and its
  own reviews. This works for closed and merged PRs too.
- `owner/name`, or a link to the repository or a file in it, is the
  repository. agent-nexus reads the head of its default branch, with the
  same read-only tools, minus `get_file_diff`.
- With no reference, agent-nexus has no code to read. It asks which
  repository a question about code is about, and answers other questions
  from what the model knows. Its chart and web tools then read only the
  hosts on `FETCH_ALLOWED_HOSTS`, since no base branch lists others.

Either way, the tools of the [MCP servers](#mcp-servers) that serve chat
are available too, such as the cluster's live state.

Only owners in `ALLOWED_OWNERS` count. `owner/name` refers to the first
configured forge (Gitea when both are); link a repository of the other
one. Naming a topic after a repository or PR (`mycroft/k8s-home#1685`)
sets it for every question in that topic.

**One pass per conversation.** Like a PR discussion, a pass reads the
conversation's latest 50 messages rather than the triggering message, and
answers every pending question in one message:

- 👀 on a question means its job is queued.
- ✅ means it is answered. Only the bot's own ✅ counts, so a question
  without it is answered by the next pass.
- If the job fails, the bot says so and leaves the question pending; the
  next question in the conversation retries it.

A newer message replaces a queued pass, and a running pass finishes, as
for PR discussions. Answers follow
[`skills/chat/SKILL.md`](skills/chat/SKILL.md).

**Access.** Anyone in the Zulip organization can ask, about any
repository of `ALLOWED_OWNERS` the forge bot can read. Answers may quote
private code into the channel they are asked in. agent-nexus never writes
to a forge from Zulip: to get a review, ask for it on the PR.

## MCP servers

The agent can call the tools of MCP servers: the live state of a
Kubernetes cluster, Prometheus metrics and Loki logs through Grafana, and
so on. Each server keeps its own credentials to its backend; agent-nexus
only reaches the server.

`MCP_CONFIG` names a YAML file listing them. The Helm chart renders
`mcp.servers` into it.

```yaml
servers:
  - name: k8s          # lowercase letters and digits, other than mcp
    url: http://kubernetes-mcp-server.mcp.svc:8080/mcp
    description: the live state of the home Kubernetes cluster, read-only
    tools: [pods_list, pods_list_in_namespace, pods_get, pods_log, events_list, resources_get, resources_list]
    jobs: [chat]       # chat, review, discussion; default chat
    load: lazy         # lazy or eager; default lazy
    timeout: 1m        # one tool call; default 1m
    tokenEnv: ""       # environment variable holding a bearer token for the server
```

What the model can reach is limited:

- **Only the listed tools.** `tools` is required: the tools the model may
  call, as `path.Match` patterns (`"*"` allows all). The others are never
  offered, and a call to one is refused. The list also bounds what the
  tools cost in context.
- **Only the listed jobs.** By default only Zulip answers get a server's
  tools. PR text is written by whoever opens the PR, so opening a server
  to reviews or discussions lets that text steer what is read and posted.
- **Read-only servers.** Run the servers read-only as well:
  - kubernetes-mcp-server with `--read-only` (`read_only = true` in its
    TOML configuration), under a ServiceAccount that cannot read Secrets;
  - mcp-grafana with `--disable-write`.
- **Data, not instructions.** The model is told that tool output, such
  as logs and annotations, is data written by others.
- **Configured connections.** The URL comes from the configuration, so it
  may be in the cluster. Redirects are not followed. The optional token
  authenticates agent-nexus to the server, for example mcp-grafana's
  `--server-auth-token`; it is the only credential agent-nexus holds for
  it.

`load` sets when the model gets a server's tools:

- **`lazy`** (the default): the prompt only names the server, with its
  description. The model gets two tools, `mcp_tools` and `mcp_call`.
  `mcp_tools` connects to a server and lists its tools with their
  parameters; `mcp_call` calls one. A server costs a line in the prompt
  until the model loads it, and is not contacted unless it does. A
  listing over 12 KiB gives the tools without their parameters, which the
  model then asks for by name.
- **`eager`**: the job connects when it starts, and the server's tools
  are the model's own, named `<server>_<tool>` (`k8s_pods_list`). Each
  request carries their schemas, but the model saves a turn, and
  llama-server holds its arguments to each tool's schema. It suits a
  small server used in most conversations.

Either way, the tools the model has never change during a job, so
llama-server keeps reusing its cached prompt.

Each job opens its own sessions, and lists each server's tools once.
agent-nexus uses the official Go SDK over Streamable HTTP, so it speaks
both the 2026-07-28 protocol and the earlier one (`initialize`), which
servers with sessions use. A server that is down is left out: the job
runs without its tools, or `mcp_tools` tells the model it is unavailable.
`agent_nexus_mcp_errors_total` counts it.

To check a configuration, list what each server gives the agent:

```sh
MCP_CONFIG=mcp.yaml agent-nexus mcp
```

It prints each allowed tool with its approximate size in tokens, what the
server costs in each prompt and, when lazy, once loaded, the tools the
list leaves out, and the patterns that match no tool.

## API

All `/v1` routes but the Zulip webhook and the runner API require
`Authorization: Bearer <api key>`.

- `POST /v1/events/pr-review` takes `{"server_url", "repo", "pr", "head_sha"}`,
  and optionally `"force": true` to review a commit that was already
  reviewed.
  - `202` with `{"job_id"}` when the review is queued.
  - `200` when it is a duplicate of a live or finished job, when the
    event is stale, or when the PR is a draft or closed.
- `POST /v1/events/pr-comment` takes `{"server_url", "repo", "pr"}`. It
  queues a pass over the pull request's discussion, which answers what is
  waiting for the bot.
  - `202` with `{"job_id"}`. A newer event replaces a queued pass; a running
    pass finishes. Closed and merged PRs are discussed too.
- `POST /v1/events/zulip` receives the Zulip bot's outgoing webhook. It
  needs no API key: Zulip authenticates with the bot's webhook token, in the
  body. It queues a pass over the message's conversation and answers
  `200` with `{"response_not_required": true}`; the job posts the answer.
  The route exists only when Zulip is configured.
- `GET /v1/jobs` lists recent jobs, newest first, with their payload
  (forge, repository, PR) and their progress so far:
  - progress fields: `turns`, `peak_prompt_tokens`, `prompt_tokens`,
    `completion_tokens`, `last_activity`;
  - filters: `?status=running,queued` (comma-separated), `?kind=pr_review`,
    `pr_discussion` or `chat_question`, `?limit=20` (at most 100).
- `GET /v1/jobs/{id}` returns the job's status, result or error, and
  token usage.
- `GET /v1/jobs/{id}/steps` returns the agent transcript: prompts, tool
  calls and answers.
- `GET /healthz` needs no key.

With `RUNNER_TOKENS`, `/v1/runner/` serves the runners instead, which
authenticate with a runner token, never an API key:

- `POST /v1/runner/claim` takes `{"protocol", "version", "runner", "model",
  "kinds"}` and waits up to 25 seconds for a job of that model: `200`
  with the job, its checkpoint, a lease and the job timeout, or `204`.
  The controller's own configuration decides what the model may run: its
  `concurrency` across all runners, and its `jobs`.
- `POST /v1/runner/jobs/{id}/heartbeat`, `/steps`, `/checkpoint`,
  `/finish` and `/release` take the lease. A lease is renewed by a
  heartbeat at least every 2 minutes, or the job is taken back. `409`
  tells the runner to stop: the job was superseded, timed out, or is no
  longer its own.
- `POST /v1/runner/enqueue` queues a job a handler starts, such as the
  review an `@agent-nexus review` comment asks for.

## Jobs UI

`UI_ADDR` serves a read-only web UI over the job store:

- the main page shows the running jobs (model, turns, peak context, last
  activity), the queued ones, each model's slots in use, and the job
  history;
- a job page shows its outcome and every step of the agent transcript:
  prompts, reasoning, tool calls and tool output.

Both pages refresh themselves with htmx while jobs run. The theme follows
the OS until toggled.

![The jobs page: a running review, the queued jobs and the history](docs/screenshots/jobs.png)

![A job page in dark mode: a review reading the values a repository overrides and diffing the chart versions of a bump](docs/screenshots/job.png)

The screenshots use demo data.

The UI has no authentication of its own, which is why it has its own
listener: route it through an authenticating proxy (an Authentik outpost,
for instance), on its own host, and keep the API's ingress pointing at
`LISTEN_ADDR` only. The API keeps authenticating with its API keys, so CI
runners reach it without going through the proxy.

## Metrics

These are served on `METRICS_ADDR` at `/metrics`:

| Metric | |
|---|---|
| `agent_nexus_jobs{status}` | Unfinished jobs: `queued`, `running`, `posting` |
| `agent_nexus_job_oldest_seconds{status}` | Age of the oldest unfinished job: since it was queued, or since it started |
| `agent_nexus_jobs_total{kind,status,model}` | Finished jobs |
| `agent_nexus_job_duration_seconds{kind,model}` | Job run time |
| `agent_nexus_model_jobs{model}` | Jobs running on each model: the slots in use |
| `agent_nexus_model_concurrency{model}` | Jobs each model may run at once |
| `agent_nexus_llm_request_duration_seconds{outcome,model}` | Chat-completion latency |
| `agent_nexus_llm_tokens_total{type,model}` | Prompt and completion tokens |
| `agent_nexus_tool_calls_total{tool,outcome}` | Tool calls: `ok`, `error`, `repeat`, `refused` |
| `agent_nexus_forge_errors_total{forge}` | Failed forge API requests |
| `agent_nexus_mcp_errors_total{server}` | MCP servers a job could not open |

For example, to alert on a stuck job, compare
`agent_nexus_job_oldest_seconds{status="running"}` with `JOB_TIMEOUT`,
which defaults to 30 minutes. Queued jobs waiting while
`agent_nexus_model_jobs` stays at `agent_nexus_model_concurrency` need
more slots, or another model.

## Deployment

The Helm chart in [`charts/agent-nexus`](charts/agent-nexus) runs a
single-replica StatefulSet, the controller, and optionally a Deployment of
runners (see [Runners](#runners)). The controller's volume holds the
SQLite job store and the repository cache. The pod runs as uid 65532 with a read-only root
filesystem.

```sh
helm install agent-nexus oci://registry.mkz.me/mycroft/charts/agent-nexus \
  --version 0.1.0 -f values.yaml
```

The chart needs an existing Secret, named by `existingSecret`, with these
keys:

- `api-keys` (required)
- `llm-api-key` (optional)
- the `apiKeyKey` of each model that has one
- `gitea-token` (when Gitea is enabled)
- `github-app-private-key` (when GitHub is enabled)
- `zulip-bot-api-key` and `zulip-webhook-token` (when Zulip is enabled)
- the `tokenKey` of each MCP server that has one
- `runner-tokens`, `name=token,…` (when `runners.enabled`), and the
  `runners.tokenKey` of the runner pods, one of those tokens (when
  `runners.replicas`)

Everything else lives in [`values.yaml`](charts/agent-nexus/values.yaml).
The chart fails to render when a required setting is missing.

The Service has three ports: `http` (the API, which the chart's ingress
routes), `metrics`, and `ui`. The chart never exposes `ui`; give it its own
ingress behind an authenticating proxy (see [Jobs UI](#jobs-ui)).

Pushing a `vX.Y.Z` tag releases both artifacts:

- **Image:** `registry.mkz.me/mycroft/agent-nexus:X.Y.Z`
- **Chart:** `oci://registry.mkz.me/mycroft/charts/agent-nexus` at version
  `X.Y.Z`, whose appVersion points at that image.

## Setup

### llama-server

- The model must support tool calls. `--jinja` is on by default in recent
  builds.
- A context of at least 32k tokens is recommended.
- If a reverse proxy sits in front of llama-server, its timeout must be at
  least `LLM_REQUEST_TIMEOUT`. llama-server queues requests while every
  slot is busy, so a request can wait a long time.

### Gitea

1. **Bot account.** Create an `agent-nexus` bot account (Site
   Administration → User Accounts → Create, user type *Bot*) and give it
   read access to the private repositories. From the bot's admin page,
   create a token with these scopes, then set `GITEA_USER` and
   `GITEA_TOKEN`:
   - `write:repository`, to fetch the code, post reviews and reply in
     threads;
   - `read:user`, to look up its own login;
   - `read:issue`, to read PR comments: mentions, and the earlier
     discussion;
   - optionally `write:issue`, to answer mentions with a PR comment
     instead of a review.
2. **Actions secret.** Add a user-level Actions secret named
   `AGENT_NEXUS_API_KEY` with one of the `API_KEYS` values.
3. **Scoped workflow.** Register
   [`.gitea/scoped_workflows/pr-review.yaml`](.gitea/scoped_workflows/pr-review.yaml)
   under User Settings → Actions → Scoped Workflows. It then runs on every
   repository you own, and a repository can opt out. It sends events to
   two endpoints:
   - pull request events to `pr-review`, including review requests
     addressed to `agent-nexus`, which are sent with `force: true`;
   - comment and review events to `pr-comment`.
4. **GitHub mirror.** Push-mirror this repository to
   `github.com/mycroft/agent-nexus`, so GitHub repositories can use the
   reusable workflow.

### GitHub

1. **Create the App.** Create a GitHub App with these permissions:
   - Contents: read
   - Pull requests: read & write
   - Metadata: read

   No webhook is needed.
2. **Configure agent-nexus.** Install the App on the repositories. Set
   `GITHUB_APP_ID` and `GITHUB_APP_PRIVATE_KEY`.
3. **Per repository.** Add the `AGENT_NEXUS_API_KEY` secret and this
   caller workflow:

```yaml
name: agent-nexus
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
  issue_comment:
    types: [created]
  pull_request_review_comment:
    types: [created]
jobs:
  agent-nexus:
    if: ${{ github.event_name != 'pull_request' || !github.event.pull_request.draft }}
    uses: mycroft/agent-nexus/.github/workflows/pr-review.yaml@main
    secrets:
      AGENT_NEXUS_API_KEY: ${{ secrets.AGENT_NEXUS_API_KEY }}
```

### Zulip

1. **Create the bot.** In Zulip, open Personal settings → Bots → Add a new
   bot:
   - bot type: *Outgoing webhook*;
   - endpoint URL: agent-nexus's `/v1/events/zulip`, e.g.
     `http://agent-nexus.agent-nexus.svc:8080/v1/events/zulip` from inside
     the cluster;
   - outgoing webhook format: *Zulip*.
2. **Configure agent-nexus.** Download the bot's zuliprc. Set `ZULIP_URL`
   to the server root, and `ZULIP_BOT_EMAIL`, `ZULIP_BOT_API_KEY` and
   `ZULIP_WEBHOOK_TOKEN` to its `email`, `key` and `token`.
3. **Private channels.** Subscribe the bot to the private channels where it
   is asked questions, so it can read their history.
4. **Network.** Zulip sends outgoing webhooks through its Smokescreen proxy,
   which refuses private addresses. If agent-nexus has one, allow it with
   `CONFIG_http_proxy__allow_addresses` (or `__allow_ranges`) in Zulip's
   environment, and let Zulip through any NetworkPolicy in front of
   agent-nexus.

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint
make build  # also builds the UI stylesheet
make css-watch
make helm-lint
CONTAINER_TOOL=podman make docker-build
```

Tests need `git` on the PATH. `make css` downloads the Tailwind standalone
CLI into `bin/` and writes `internal/web/static/css/app.css`, which is not
committed. htmx is vendored in `internal/web/static/js` (`make vendor-js`
updates it).
