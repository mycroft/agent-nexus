// Package review implements pull request reviews: it gathers the pull
// request and its diff, runs the agent with the pr-review skill and
// read-only repository tools, and posts the result as a forge review.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/agent"
	"github.com/mycroft/agent-nexus/internal/charts"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/mcp"
	"github.com/mycroft/agent-nexus/internal/remote"
	"github.com/mycroft/agent-nexus/internal/skill"
)

// Kind is the job kind handled by this package.
const Kind = "pr_review"

const (
	maxToolOutput = 16 << 10
	headPeekBytes = 1 << 10
)

// guidanceFiles are read from the base branch, first match wins.
var guidanceFiles = []string{"AGENTS.md", "CLAUDE.md"}

// Request identifies the commit of a pull request to review. It is the
// job payload.
type Request struct {
	Forge   string `json:"forge"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	PR      int    `json:"pr"`
	HeadSHA string `json:"head_sha"`
	// Force reviews the commit again even if it was reviewed already, as
	// asked by a review request or an "@agent-nexus review" comment.
	Force bool `json:"force,omitempty"`
}

func (r Request) repo() forge.Repo { return forge.Repo{Owner: r.Owner, Name: r.Repo} }

// GroupKey groups the review jobs of one pull request: a newer one
// supersedes the older unfinished ones.
func GroupKey(forgeName string, repo forge.Repo, pr int) string {
	return fmt.Sprintf("%s:%s#%d", forgeName, strings.ToLower(repo.String()), pr)
}

// Job builds the job reviewing req. One review job lives per commit;
// a forced review gets a key of its own, so it runs even after one.
func Job(req Request, apiKeyName string) jobs.NewJob {
	group := GroupKey(req.Forge, req.repo(), req.PR)
	dedupe := group + "@" + req.HeadSHA
	if req.Force {
		dedupe += fmt.Sprintf("@forced@%d", time.Now().UnixNano())
	}
	return jobs.NewJob{Kind: Kind, GroupKey: group, DedupeKey: dedupe, APIKeyName: apiKeyName, Payload: req}
}

// reviewMarker tags a review with what it covers: the commit, and for a
// forced review the job, so a forced review is not mistaken for the
// commit's earlier one.
func reviewMarker(req Request, jobID int64) string {
	if req.Force {
		return fmt.Sprintf("<!-- agent-nexus:sha=%s:job=%d -->", req.HeadSHA, jobID)
	}
	return marker(req.HeadSHA)
}

// Outcome is what happened to a review request.
type Outcome string

// Outcomes.
const (
	Posted        Outcome = "posted"
	AlreadyPosted Outcome = "already_posted"
	Drafted       Outcome = "drafted"
	SkippedClosed Outcome = "skipped_closed"
	SkippedDraft  Outcome = "skipped_draft"
	Stale         Outcome = "superseded"
)

// Draft is a finished review not yet posted. It is checkpointed so a
// crash between generating and posting does not redo the LLM work.
type Draft struct {
	HeadSHA      string          `json:"head_sha"`
	Review       forge.NewReview `json:"review"`
	FallbackBody string          `json:"fallback_body"`
	Output       Output          `json:"output"`
	Usage        llm.Usage       `json:"usage"`
	PeakPrompt   int             `json:"peak_prompt_tokens"`
	Turns        int             `json:"turns"`
	// Marker identifies the posted review (see reviewMarker).
	Marker     string `json:"marker"`
	Mode       Mode   `json:"mode"`
	ModeSource string `json:"mode_source"`
}

// Reviewer produces and posts reviews.
type Reviewer struct {
	Forges map[string]forge.Forge
	Repos  *gitrepo.Cache
	LLM    agent.Completer
	Model  string
	Skill  skill.Skill
	// DiscussionSkill guides answers to replies and mentions.
	DiscussionSkill skill.Skill
	ContextTokens   int
	MaxTurns        int
	// ModeRules is the central review mode configuration (see resolveMode).
	ModeRules []ModeRule
	// Charts, if set, gives the agent the chart tools. They read the chart
	// repositories the base branch references, and those on FetchHosts.
	Charts *charts.Fetcher
	// Remote, if set, gives the agent fetch_url and release_notes, for
	// the hosts and images the base branch references, and FetchHosts.
	Remote     *remote.Client
	FetchHosts []string
	// MCP are the configured MCP servers. Each job gets the tools of those
	// serving it (see openMCP).
	MCP []*mcp.Client
	// Enqueue queues a job; discussions use it to start a review asked
	// for in a comment. Without it, such comments are answered like
	// questions.
	Enqueue func(context.Context, jobs.NewJob) (jobs.Job, bool, error)
	Logger  *slog.Logger
}

// WithModel returns a copy of r whose jobs run on the given model: llm
// answers as model (the upstream identifier, shown in the review footer)
// within contextTokens.
func (r *Reviewer) WithModel(llm agent.Completer, model string, contextTokens int) *Reviewer {
	c := *r
	c.LLM, c.Model, c.ContextTokens = llm, model, contextTokens
	return &c
}

// Handle implements jobs.Handler.
func (r *Reviewer) Handle(ctx context.Context, job *jobs.Job, run *jobs.Run) (jobs.Result, error) {
	var req Request
	if err := json.Unmarshal(job.Payload, &req); err != nil {
		return jobs.Result{}, fmt.Errorf("decode review request: %w", err)
	}

	draft, outcome, err := r.resumeOrDraft(ctx, req, job, run)
	if err != nil {
		return jobs.Result{}, err
	}
	if draft == nil {
		return outcomeResult(outcome), nil
	}

	outcome, err = r.Post(ctx, req, draft)
	if err != nil {
		return jobs.Result{}, err
	}
	res := outcomeResult(outcome)
	res.Data = map[string]any{
		"outcome":            outcome,
		"verdict":            draft.Output.Verdict,
		"findings":           len(draft.Output.Findings),
		"inline_comments":    len(draft.Review.Comments),
		"prompt_tokens":      draft.Usage.PromptTokens,
		"completion_tokens":  draft.Usage.CompletionTokens,
		"peak_prompt_tokens": draft.PeakPrompt,
		"mode":               draft.Mode,
		"mode_source":        draft.ModeSource,
		"event":              draft.Review.Event,
		"turns":              draft.Turns,
	}
	return res, nil
}

// resumeOrDraft returns the checkpointed draft of a job interrupted while
// posting, or drafts the review and checkpoints it.
func (r *Reviewer) resumeOrDraft(ctx context.Context, req Request, job *jobs.Job, run *jobs.Run) (*Draft, Outcome, error) {
	if len(job.Checkpoint) > 0 {
		var draft Draft
		if err := json.Unmarshal(job.Checkpoint, &draft); err != nil {
			return nil, "", fmt.Errorf("decode checkpoint: %w", err)
		}
		return &draft, Drafted, nil
	}
	record := func(st agent.Step) {
		run.Record(ctx, jobs.Step{
			Role: st.Role, ToolName: st.ToolName, Content: st.Content,
			PromptTokens: st.PromptTokens, CompletionTokens: st.CompletionTokens,
		})
	}
	draft, outcome, err := r.Draft(ctx, req, job.ID, record)
	if err != nil || draft == nil {
		return nil, outcome, err
	}
	if err := run.Checkpoint(ctx, draft); err != nil {
		return nil, "", err
	}
	return draft, outcome, nil
}

func outcomeResult(o Outcome) jobs.Result {
	data := map[string]any{"outcome": o}
	switch o {
	case Stale:
		return jobs.Result{Status: jobs.Superseded, Data: data}
	case SkippedClosed, SkippedDraft:
		return jobs.Result{Status: jobs.Skipped, Data: data}
	default:
		return jobs.Result{Status: jobs.Succeeded, Data: data}
	}
}

func (r *Reviewer) forge(name string) (forge.Forge, error) {
	f, ok := r.Forges[name]
	if !ok {
		return nil, fmt.Errorf("forge %q is not configured", name)
	}
	return f, nil
}

// checkPR returns a non-empty outcome when the pull request should not be
// reviewed at headSHA.
func checkPR(pr forge.PR, headSHA string) Outcome {
	switch {
	case pr.State != "open":
		return SkippedClosed
	case pr.Draft:
		return SkippedDraft
	case pr.HeadSHA != headSHA:
		return Stale
	}
	return ""
}

// Draft runs the review and returns it without posting. A nil draft comes
// with the outcome explaining why no review was produced.
func (r *Reviewer) Draft(ctx context.Context, req Request, jobID int64, record func(agent.Step)) (*Draft, Outcome, error) {
	f, err := r.forge(req.Forge)
	if err != nil {
		return nil, "", err
	}
	repoID := req.repo()
	pr, err := f.GetPR(ctx, repoID, req.PR)
	if err != nil {
		return nil, "", err
	}
	if o := checkPR(pr, req.HeadSHA); o != "" {
		return nil, o, nil
	}

	ws, outcome, err := r.prepare(ctx, f, repoID, pr, req.HeadSHA)
	if err != nil || ws == nil {
		return nil, outcome, err
	}
	mode, modeSource, configWarning := r.reviewMode(ctx, ws.repo, ws.baseSHA, repoID)
	r.logger().InfoContext(ctx, "review mode", "repo", repoID.String(), "mode", mode, "source", modeSource)

	runner := &agent.Runner{
		LLM:           r.LLM,
		ContextTokens: r.ContextTokens,
		MaxTurns:      r.MaxTurns,
		MaxToolOutput: maxToolOutput,
		Record:        record,
		Logger:        r.Logger,
	}
	mcpTools, mcpSentence, closeMCP := r.openMCP(ctx, mcp.JobReview)
	defer closeMCP()
	system := systemPrompt(r.Skill.Body, repoID, f.Name(), pr, withMCP(ws.toolsSentence(), mcpSentence), ws.guidanceName, ws.guidance)
	user := userPrompt(repoID, f.Name(), pr, ws.baseSHA, ws.changed(ctx)) + r.earlierDiscussion(ctx, f, repoID, pr.Number)
	session, err := runner.Run(ctx, system, user, append(ws.tools(), mcpTools...))
	if err != nil {
		return nil, "", fmt.Errorf("agent: %w", err)
	}

	var out Output
	err = session.Finish(ctx, finalInstruction(session.TurnLimitReached),
		llm.JSONSchema{Name: "pr_review", Schema: outputSchema},
		func(raw []byte) error {
			var err error
			out, err = parseOutput(raw)
			return err
		})
	if err != nil {
		return nil, "", fmt.Errorf("agent: %w", err)
	}
	out.Findings = cleanFindings(out.Findings, r.Logger)

	var v *verdict
	if mode == ModeVerdict {
		event, note := verdictEvent(out)
		v = &verdict{Event: event, Reason: out.VerdictReason, Note: note}
	}
	review, fallback := render(out, ws.files, footer{
		HeadSHA:          req.HeadSHA,
		Model:            r.Model,
		Turns:            session.Turns,
		PeakPromptTokens: session.PeakPromptTokens,
		CompletionTokens: session.Usage.CompletionTokens,
		JobID:            jobID,
		Marker:           reviewMarker(req, jobID),
		Warning:          configWarning,
	}, v)
	return &Draft{
		HeadSHA:      req.HeadSHA,
		Review:       review,
		FallbackBody: fallback,
		Output:       out,
		Usage:        session.Usage,
		PeakPrompt:   session.PeakPromptTokens,
		Turns:        session.Turns,
		Marker:       reviewMarker(req, jobID),
		Mode:         mode,
		ModeSource:   modeSource,
	}, Drafted, nil
}

// reviewMode resolves the review mode of a repository from the central
// rules and its config file at the base commit. An unreadable or invalid
// file is ignored, with a warning for the review body.
func (r *Reviewer) reviewMode(ctx context.Context, repo *gitrepo.Repo, baseSHA string, repoID forge.Repo) (Mode, string, string) {
	var fileMode Mode
	warning := ""
	content, err := repo.ReadFile(ctx, baseSHA, RepoConfigFile)
	switch {
	case errors.Is(err, gitrepo.ErrNotFound):
	case err != nil:
		warning = fmt.Sprintf("%s could not be read and was ignored: %v", RepoConfigFile, err)
	default:
		if fileMode, err = parseRepoConfig(content); err != nil {
			warning = fmt.Sprintf("%s was ignored: %v", RepoConfigFile, err)
		}
	}
	if warning != "" {
		r.logger().WarnContext(ctx, "repository config ignored", "repo", repoID.String(), "warning", warning)
	}
	mode, source := resolveMode(r.ModeRules, repoID, fileMode)
	return mode, source, warning
}

// marker returns the draft's marker; checkpoints from before markers were
// stored carry the commit's.
func (d *Draft) marker() string {
	if d.Marker != "" {
		return d.Marker
	}
	return marker(d.HeadSHA)
}

// cleanFindings normalizes paths and drops findings without evidence, which
// the skill forbids shipping.
func cleanFindings(findings []Finding, log *slog.Logger) []Finding {
	out := findings[:0]
	for _, f := range findings {
		if strings.TrimSpace(f.Evidence) == "" {
			if log != nil {
				log.Info("dropping finding without evidence", "title", f.Title)
			}
			continue
		}
		f.Path = strings.TrimPrefix(strings.TrimLeft(strings.TrimPrefix(f.Path, "b/"), "/"), "./")
		f.Line = max(f.Line, 0)
		out = append(out, f)
	}
	return out
}

// Post publishes a draft unless the pull request moved on or the review is
// already there. It is safe to call again after a crash.
func (r *Reviewer) Post(ctx context.Context, req Request, d *Draft) (Outcome, error) {
	f, err := r.forge(req.Forge)
	if err != nil {
		return "", err
	}
	repoID := req.repo()
	pr, err := f.GetPR(ctx, repoID, req.PR)
	if err != nil {
		return "", err
	}
	if o := checkPR(pr, d.HeadSHA); o != "" {
		return o, nil
	}
	posted, err := r.alreadyPosted(ctx, f, repoID, req.PR, d.marker())
	if err != nil {
		return "", err
	}
	if posted {
		return AlreadyPosted, nil
	}
	if err := f.DeletePendingReviews(ctx, repoID, req.PR); err != nil {
		return "", err
	}

	for i, review := range postAttempts(d) {
		if i > 0 {
			// A failed attempt may still have gone through, or left a
			// pending review behind.
			r.logger().WarnContext(ctx, "review rejected, retrying a simpler one", "error", err, "event", review.Event, "inline_comments", len(review.Comments))
			posted, perr := r.alreadyPosted(ctx, f, repoID, req.PR, d.marker())
			if perr != nil {
				return "", perr
			}
			if posted {
				return AlreadyPosted, nil
			}
			if derr := f.DeletePendingReviews(ctx, repoID, req.PR); derr != nil {
				return "", derr
			}
		}
		if err = f.CreateReview(ctx, repoID, req.PR, review); err == nil {
			return Posted, nil
		}
	}
	return "", err
}

// postAttempts lists the reviews to try in order: the draft; then, as
// inline comments are the usual cause of a rejection, every finding in the
// body; then, in case the bot may not approve or request changes, the same
// as a plain comment.
func postAttempts(d *Draft) []forge.NewReview {
	attempts := []forge.NewReview{d.Review}
	if len(d.Review.Comments) > 0 {
		attempts = append(attempts, forge.NewReview{CommitSHA: d.HeadSHA, Event: d.Review.Event, Body: d.FallbackBody})
	}
	if d.Review.Event != "" && d.Review.Event != forge.EventComment {
		attempts = append(attempts, forge.NewReview{CommitSHA: d.HeadSHA, Event: forge.EventComment, Body: d.FallbackBody})
	}
	return attempts
}

func (r *Reviewer) alreadyPosted(ctx context.Context, f forge.Forge, repo forge.Repo, pr int, mark string) (bool, error) {
	login, err := f.BotLogin(ctx)
	if err != nil {
		return false, err
	}
	reviews, err := f.ListReviews(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	for _, rv := range reviews {
		if strings.EqualFold(rv.User, login) && rv.State != "PENDING" && strings.Contains(rv.Body, mark) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Reviewer) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
