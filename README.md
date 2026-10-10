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
- **Comment only.** Reviews are always posted as `COMMENT`. The verdict is
  text in the body, never an approval or a change request.

The review instructions live in [`skills/pr-review/SKILL.md`](skills/pr-review/SKILL.md),
which uses the Agent Skills format and is embedded in the binary.

## Commands

```sh
agent-nexus serve      # HTTP API + workers
agent-nexus review -forge gitea -repo mycroft/k8s-home -pr 42 -dry-run -transcript
agent-nexus version
```

`review` runs one review synchronously and prints it. With `-dry-run` it
does not post, which makes it the quickest way to iterate on the skill.
Point `SKILLS_DIR=./skills` at the working tree to skip rebuilding.

## Configuration

Everything is configured through environment variables.

| Variable | Default | |
|---|---|---|
| `LLM_BASE_URL` | required | OpenAI-compatible root, e.g. `https://llm-api.iop.cx/v1` |
| `LLM_API_KEY` | | Bearer token for the LLM server |
| `LLM_MODEL` | required | Model name sent with each request |
| `LLM_CONTEXT_TOKENS` | from `/props` | Context window; set it to avoid the startup lookup |
| `LLM_CONCURRENCY` | `1` | Parallel jobs; match llama-server `--parallel` |
| `LLM_REQUEST_TIMEOUT` | `10m` | One completion, including time queued in llama-server |
| `AGENT_MAX_TURNS` | `30` | Tool-loop turns per review |
| `JOB_TIMEOUT` | `30m` | One job end to end |
| `API_KEYS` | required for `serve` | `name=key,name=key`, each key ≥ 32 characters |
| `ALLOWED_OWNERS` | required for `serve` | Comma-separated repository owners |
| `EVENTS_PER_MINUTE` | `30` | Rate limit per API key |
| `GITEA_URL`, `GITEA_USER`, `GITEA_TOKEN` | | Gitea forge (bot account) |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY` | | GitHub forge (App, PEM key) |
| `GITHUB_API_URL`, `GITHUB_SERVER_URL` | github.com | For GitHub Enterprise |
| `LISTEN_ADDR` | `:8080` | API |
| `METRICS_ADDR` | `:9090` | `/metrics`; keep it out of the Ingress |
| `DB_PATH` | `agent-nexus.db` | SQLite database |
| `REPO_CACHE_DIR` | `repos` | Bare repository cache |
| `TRANSCRIPT_RETENTION` | `720h` | How long agent transcripts are kept |
| `SKILLS_DIR` | embedded | Load skills from disk instead |
| `LOG_LEVEL` | `info` | |

At least one forge must be configured.

## API

All `/v1` routes require `Authorization: Bearer <api key>`.

- `POST /v1/events/pr-review` takes `{"server_url", "repo", "pr", "head_sha"}`.
  - `202` with `{"job_id"}` when the review is queued.
  - `200` when it is a duplicate of a live or finished job, when the
    event is stale, or when the PR is a draft or closed.
- `GET /v1/jobs/{id}` returns the job's status, result or error, and
  token usage.
- `GET /v1/jobs/{id}/steps` returns the agent transcript: prompts, tool
  calls and answers.
- `GET /healthz` needs no key.

## Deployment

The Helm chart in [`charts/agent-nexus`](charts/agent-nexus) runs a
single-replica StatefulSet. Its volume holds the SQLite job store and the
repository cache. The pod runs as uid 65532 with a read-only root
filesystem.

```sh
helm install agent-nexus oci://registry.mkz.me/mycroft/charts/agent-nexus \
  --version 0.1.0 -f values.yaml
```

The chart needs an existing Secret, named by `existingSecret`, with these
keys:

- `api-keys` (required)
- `llm-api-key` (optional)
- `gitea-token` (when Gitea is enabled)
- `github-app-private-key` (when GitHub is enabled)

Everything else lives in [`values.yaml`](charts/agent-nexus/values.yaml).
The chart fails to render when a required setting is missing.

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
   create a token with `write:repository` scope (to fetch the code and post
   reviews) and `read:user` scope (to look up its own login). Set
   `GITEA_USER` and `GITEA_TOKEN`.
2. **Actions secret.** Add a user-level Actions secret named
   `AGENT_NEXUS_API_KEY` with one of the `API_KEYS` values.
3. **Scoped workflow.** Register
   [`.gitea/scoped_workflows/pr-review.yaml`](.gitea/scoped_workflows/pr-review.yaml)
   under User Settings → Actions → Scoped Workflows. It then runs on every
   repository you own, and a repository can opt out.
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
name: Review
on:
  pull_request:
    types: [opened, synchronize, reopened, ready_for_review]
jobs:
  review:
    if: ${{ !github.event.pull_request.draft }}
    uses: mycroft/agent-nexus/.github/workflows/pr-review.yaml@main
    secrets:
      AGENT_NEXUS_API_KEY: ${{ secrets.AGENT_NEXUS_API_KEY }}
```

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint
make build
make helm-lint
CONTAINER_TOOL=podman make docker-build
```

Tests need `git` on the PATH.
