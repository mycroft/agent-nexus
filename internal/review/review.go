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

	"github.com/mycroft/agent-nexus/internal/agent"
	"github.com/mycroft/agent-nexus/internal/diff"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/gitrepo"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/skill"
	"github.com/mycroft/agent-nexus/internal/tools"
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
}

func (r Request) repo() forge.Repo { return forge.Repo{Owner: r.Owner, Name: r.Repo} }

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
}

// Reviewer produces and posts reviews.
type Reviewer struct {
	Forges        map[string]forge.Forge
	Repos         *gitrepo.Cache
	LLM           agent.Completer
	Model         string
	Skill         skill.Skill
	ContextTokens int
	MaxTurns      int
	Logger        *slog.Logger
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

	remote, err := f.GitRemote(ctx, repoID)
	if err != nil {
		return nil, "", err
	}
	repo, err := r.Repos.Sync(ctx, remote, pr.BaseRef, pr.Number)
	if err != nil {
		return nil, "", err
	}
	if !repo.HasCommit(ctx, req.HeadSHA) {
		// The pull request was force-pushed between reading it and
		// fetching: a newer event covers the new head.
		return nil, Stale, nil
	}
	baseSHA, err := repo.ResolveBranch(ctx, pr.BaseRef)
	if err != nil {
		return nil, "", err
	}
	rawDiff, err := repo.Diff(ctx, baseSHA, req.HeadSHA)
	if errors.Is(err, gitrepo.ErrTruncated) {
		return nil, "", fmt.Errorf("diff exceeds %d bytes; too large to review", gitrepo.MaxOutput)
	}
	if err != nil {
		return nil, "", err
	}
	files, err := diff.Parse(rawDiff)
	if err != nil {
		return nil, "", fmt.Errorf("parse diff: %w", err)
	}

	peek := func(path string) []byte {
		content, err := repo.ReadFile(ctx, req.HeadSHA, path)
		if err != nil {
			return nil
		}
		return content[:min(len(content), headPeekBytes)]
	}
	budget := int(float64(r.ContextTokens) * diffBudgetShare * charsPerToken)
	changed := selectDiffs(files, budget, peek)

	guidanceName, guidance := "", ""
	for _, name := range guidanceFiles {
		if content, err := repo.ReadFile(ctx, baseSHA, name); err == nil {
			guidanceName, guidance = name, string(content)
			break
		}
	}

	byPath := map[string]*diff.File{}
	for _, file := range files {
		byPath[file.Path()] = file
		if file.Status == diff.Renamed {
			byPath[file.OldPath] = file
		}
	}
	fileDiff := func(path string) (string, bool) {
		file, ok := byPath[path]
		if !ok {
			return "", false
		}
		if file.Binary {
			return "binary file; no textual diff", true
		}
		return file.Annotated(), true
	}

	runner := &agent.Runner{
		LLM:           r.LLM,
		ContextTokens: r.ContextTokens,
		MaxTurns:      r.MaxTurns,
		MaxToolOutput: maxToolOutput,
		Record:        record,
		Logger:        r.Logger,
	}
	system := systemPrompt(r.Skill.Body, repoID, f.Name(), pr, guidanceName, guidance)
	user := userPrompt(repoID, f.Name(), pr, baseSHA, changed)
	session, err := runner.Run(ctx, system, user, tools.Repo(repo, req.HeadSHA, fileDiff))
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

	review, fallback := render(out, files, footer{
		HeadSHA:          req.HeadSHA,
		Model:            r.Model,
		Turns:            session.Turns,
		PeakPromptTokens: session.PeakPromptTokens,
		CompletionTokens: session.Usage.CompletionTokens,
		JobID:            jobID,
	})
	return &Draft{
		HeadSHA:      req.HeadSHA,
		Review:       review,
		FallbackBody: fallback,
		Output:       out,
		Usage:        session.Usage,
		PeakPrompt:   session.PeakPromptTokens,
		Turns:        session.Turns,
	}, Drafted, nil
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
	posted, err := r.alreadyPosted(ctx, f, repoID, req.PR, d.HeadSHA)
	if err != nil {
		return "", err
	}
	if posted {
		return AlreadyPosted, nil
	}
	if err := f.DeletePendingReviews(ctx, repoID, req.PR); err != nil {
		return "", err
	}

	err = f.CreateReview(ctx, repoID, req.PR, d.Review)
	if err == nil {
		return Posted, nil
	}
	if len(d.Review.Comments) == 0 {
		return "", err
	}
	// Inline comments are the usual cause of a rejected review; retry
	// once with every finding in the body, unless the first attempt
	// actually went through.
	r.logger().WarnContext(ctx, "review with inline comments rejected, retrying without them", "error", err)
	posted, err = r.alreadyPosted(ctx, f, repoID, req.PR, d.HeadSHA)
	if err != nil {
		return "", err
	}
	if posted {
		return AlreadyPosted, nil
	}
	if err := f.DeletePendingReviews(ctx, repoID, req.PR); err != nil {
		return "", err
	}
	if err := f.CreateReview(ctx, repoID, req.PR, forge.NewReview{CommitSHA: d.HeadSHA, Body: d.FallbackBody}); err != nil {
		return "", err
	}
	return Posted, nil
}

func (r *Reviewer) alreadyPosted(ctx context.Context, f forge.Forge, repo forge.Repo, pr int, headSHA string) (bool, error) {
	login, err := f.BotLogin(ctx)
	if err != nil {
		return false, err
	}
	reviews, err := f.ListReviews(ctx, repo, pr)
	if err != nil {
		return false, err
	}
	for _, rv := range reviews {
		if strings.EqualFold(rv.User, login) && rv.State != "PENDING" && strings.Contains(rv.Body, marker(headSHA)) {
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
