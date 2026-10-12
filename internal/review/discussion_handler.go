package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/mycroft/agent-nexus/internal/agent"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/mcp"
)

// DiscussionKind is the job kind answering replies and mentions.
const DiscussionKind = "pr_discussion"

// discussionMaxTurns bounds the investigation behind one answer.
const discussionMaxTurns = 12

// DiscussionRequest identifies the pull request whose discussion to
// answer. It is the job payload.
type DiscussionRequest struct {
	Forge string `json:"forge"`
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	PR    int    `json:"pr"`
}

var replySchema = json.RawMessage(`{
  "type": "object",
  "properties": {"reply": {"type": "string"}},
  "required": ["reply"]
}`)

// settlingReplySchema is the answer's schema when the prompt lists open
// findings: it also names those the conversation settles.
var settlingReplySchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reply": {"type": "string"},
    "resolved": {"type": "array", "items": {"type": "integer"}}
  },
  "required": ["reply", "resolved"]
}`)

// DiscussionHandler returns the handler of DiscussionKind jobs.
func (r *Reviewer) DiscussionHandler() jobs.Handler { return discussionHandler{r} }

type discussionHandler struct{ r *Reviewer }

// Handle answers the pending replies and mentions of a pull request, then
// approves it if the discussion settled every finding of the latest review
// (see approveIfSettled). It reads the whole discussion rather than the
// comment that triggered it: the event may not identify the comment, and a
// missed event is caught up by the next one.
func (h discussionHandler) Handle(ctx context.Context, job *jobs.Job, run *jobs.Run) (jobs.Result, error) {
	r := h.r
	var req DiscussionRequest
	if err := json.Unmarshal(job.Payload, &req); err != nil {
		return jobs.Result{}, fmt.Errorf("decode discussion request: %w", err)
	}
	f, err := r.forge(req.Forge)
	if err != nil {
		return jobs.Result{}, err
	}
	repoID := forge.Repo{Owner: req.Owner, Name: req.Repo}
	// Closed pull requests are discussed too: people ask about a merged
	// change. Only reviews and approvals are for open ones.
	pr, err := f.GetPR(ctx, repoID, req.PR)
	if err != nil {
		return jobs.Result{}, err
	}
	login, err := f.BotLogin(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	notes, err := f.ListNotes(ctx, repoID, req.PR)
	if err != nil {
		return jobs.Result{}, err
	}
	// The checkout is only needed to answer with the model, or to read
	// the review mode before approving.
	var ws *workspace
	checkout := func() (*workspace, error) {
		if ws != nil {
			return ws, nil
		}
		w, _, err := r.prepare(ctx, f, repoID, pr, pr.HeadSHA)
		if err == nil && w == nil {
			err = errors.New("the pull request head moved while fetching it")
		}
		ws = w
		return w, err
	}
	asks := pendingAsks(notes, login)
	if len(asks) == 0 {
		// A verdict update that failed in an earlier pass is retried.
		approved, err := r.approveIfSettled(ctx, f, repoID, pr, latestReview(notes, login, pr.HeadSHA), checkout, job.ID)
		if err != nil {
			return jobs.Result{}, err
		}
		if approved {
			return jobs.Result{Status: jobs.Succeeded, Data: map[string]any{"outcome": "approved", "approved": true}}, nil
		}
		return jobs.Result{Status: jobs.Skipped, Data: map[string]any{"outcome": "nothing_to_answer"}}, nil
	}

	command := reviewCommandPattern(login)
	record := func(st agent.Step) {
		run.Record(ctx, jobs.Step{
			Role: st.Role, ToolName: st.ToolName, Content: st.Content,
			PromptTokens: st.PromptTokens, CompletionTokens: st.CompletionTokens,
		})
	}
	history := renderHistory(notes, login)
	review := latestReview(notes, login, pr.HeadSHA)
	batch := asks[:min(len(asks), maxAnswersPerJob)]
	answered, answeredElsewhere := 0, 0
	for _, a := range batch {
		var text string
		var resolved []int
		if r.Enqueue != nil && command.MatchString(a.covered[len(a.covered)-1].Body) {
			text, err = r.requestReview(ctx, f.Name(), repoID, pr, a)
		} else {
			var w *workspace
			if w, err = checkout(); err == nil {
				text, resolved, err = r.answer(ctx, f.Name(), repoID, pr, w, a, login, history, review, record)
			}
		}
		if err != nil {
			return jobs.Result{}, err
		}
		settles := ""
		if len(resolved) > 0 {
			settles = resolvesMarker(review.Key, resolved)
		}
		posted, err := r.postAnswer(ctx, f, repoID, pr, a, login, text, settles, job.ID)
		if err != nil {
			return jobs.Result{}, err
		}
		if posted {
			answered++
			for _, n := range resolved {
				review.Resolved[n] = true
			}
		} else {
			answeredElsewhere++
		}
	}

	if notes, err = f.ListNotes(ctx, repoID, req.PR); err != nil {
		return jobs.Result{}, err
	}
	approved, err := r.approveIfSettled(ctx, f, repoID, pr, latestReview(notes, login, pr.HeadSHA), checkout, job.ID)
	if err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{Status: jobs.Succeeded, Data: map[string]any{
		"outcome":            "answered",
		"answered":           answered,
		"answered_elsewhere": answeredElsewhere,
		// Left for the next pass, beyond this pass's batch.
		"remaining": len(asks) - len(batch),
		"approved":  approved,
	}}, nil
}

// reviewCommandPattern matches a comment asking the bot for a review:
// "@agent-nexus review", "@agent-nexus review again", "@agent-nexus re-review".
func reviewCommandPattern(login string) *regexp.Regexp {
	name := regexp.QuoteMeta(strings.TrimSuffix(login, "[bot]"))
	return regexp.MustCompile(`(?i)(^|[^\w.-])@` + name + `[,:]?\s+(re-?)?review\b`)
}

// requestReview queues a forced review of the pull request's head, as
// asked in a comment, and returns the answer saying so.
func (r *Reviewer) requestReview(ctx context.Context, forgeName string, repoID forge.Repo, pr forge.PR, a ask) (string, error) {
	switch {
	case pr.State != "open":
		return "This pull request is closed: only open pull requests are reviewed. Ask me about it here instead.", nil
	case pr.Draft:
		return "This pull request is a draft: it is reviewed once it is ready.", nil
	}
	asker := a.covered[len(a.covered)-1].User
	job, _, err := r.Enqueue(ctx, Job(Request{
		Forge: forgeName, Owner: repoID.Owner, Repo: repoID.Name,
		PR: pr.Number, HeadSHA: pr.HeadSHA, Force: true,
	}, "comment:"+asker))
	if err != nil {
		return "", fmt.Errorf("queue the requested review: %w", err)
	}
	return fmt.Sprintf("Queued a new review of `%s` (job %d).", short(pr.HeadSHA), job.ID), nil
}

// answer runs the agent on one conversation and returns its reply, and the
// open findings of the latest review (numbered from 1) it settles.
func (r *Reviewer) answer(ctx context.Context, forgeName string, repoID forge.Repo, pr forge.PR, ws *workspace, a ask, login, history string, review *reviewState, record func(agent.Step)) (string, []int, error) {
	runner := &agent.Runner{
		LLM:           r.LLM,
		ContextTokens: r.ContextTokens,
		MaxTurns:      min(r.MaxTurns, discussionMaxTurns),
		MaxToolOutput: maxToolOutput,
		Record:        record,
		Logger:        r.Logger,
	}
	mcpTools, mcpSentence, closeMCP := r.openMCP(ctx, mcp.JobDiscussion)
	defer closeMCP()
	system := discussionSystemPrompt(r.DiscussionSkill.Body, repoID, forgeName, pr, withMCP(ws.toolsSentence(), mcpSentence), ws.guidanceName, ws.guidance)
	findings := findingsSection(review)
	user := discussionUserPrompt(repoID, forgeName, pr, ws, a, login, history) + findings
	session, err := runner.Run(ctx, system, user, append(ws.tools(), mcpTools...))
	if err != nil {
		return "", nil, fmt.Errorf("agent: %w", err)
	}
	schema := replySchema
	if findings != "" {
		schema = settlingReplySchema
	}
	var reply string
	var resolved []int
	err = session.Finish(ctx, discussionFinalInstruction(session.TurnLimitReached, findings != ""),
		llm.JSONSchema{Name: "pr_discussion_reply", Schema: schema},
		func(raw []byte) error {
			var out struct {
				Reply    string `json:"reply"`
				Resolved []int  `json:"resolved"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			if reply = strings.TrimSpace(out.Reply); reply == "" {
				return errors.New("reply is empty")
			}
			resolved = out.Resolved
			return nil
		})
	if err != nil {
		return "", nil, fmt.Errorf("agent: %w", err)
	}
	if findings == "" {
		return reply, nil, nil
	}
	return reply, review.openFindings(resolved), nil
}

// postAnswer posts the reply in the thread, or as a pull request comment
// for a mention, unless another job answered meanwhile. settles is the
// marker of the findings the answer settles, if any. It reports whether it
// posted.
func (r *Reviewer) postAnswer(ctx context.Context, f forge.Forge, repoID forge.Repo, pr forge.PR, a ask, login, text, settles string, jobID int64) (bool, error) {
	number := pr.Number
	notes, err := f.ListNotes(ctx, repoID, number)
	if err != nil {
		return false, err
	}
	answered := answeredKeys(notes, login)
	for _, n := range a.covered {
		if answered[n.Key()] {
			return false, nil
		}
	}

	body := replyBody(a, text, settles, jobID)
	if a.inline {
		return true, f.ReplyInline(ctx, repoID, number, a.thread[0], body)
	}
	err = f.Comment(ctx, repoID, number, body)
	var statusErr *httpclient.StatusError
	if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusForbidden || statusErr.StatusCode == http.StatusUnauthorized) {
		// Pull request comments need more token scopes than reviews
		// (Gitea: write:issue). A comment-only review carries the
		// answer instead.
		r.logger().WarnContext(ctx, "cannot comment, answering with a review", "error", err)
		// As for reviews: a failed submission may have left a pending
		// review, which the next one would silently join.
		if err := f.DeletePendingReviews(ctx, repoID, number); err != nil {
			return false, err
		}
		return true, f.CreateReview(ctx, repoID, number, forge.NewReview{CommitSHA: pr.HeadSHA, Event: forge.EventComment, Body: body})
	}
	return err == nil, err
}

// replyBody formats an answer: a quote of the note for a mention (it is
// posted apart from it), the text, a footer, one marker per covered note,
// and the marker of the findings it settles.
func replyBody(a ask, text, settles string, jobID int64) string {
	var b strings.Builder
	if !a.inline {
		n := a.covered[0]
		quote := truncate(strings.TrimSpace(n.Body), 300)
		fmt.Fprintf(&b, "> **@%s**:\n", n.User)
		for line := range strings.SplitSeq(quote, "\n") {
			b.WriteString("> " + line + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(strings.TrimSpace(text))
	fmt.Fprintf(&b, "\n\n<sub>agent-nexus · job %d</sub>\n", jobID)
	for _, n := range a.covered {
		b.WriteString(replyMarker(n) + "\n")
	}
	if settles != "" {
		b.WriteString(settles + "\n")
	}
	return b.String()
}

func discussionSystemPrompt(skillBody string, repo forge.Repo, forgeName string, pr forge.PR, tools, guidanceName, guidance string) string {
	var b strings.Builder
	b.WriteString(skillBody)
	fmt.Fprintf(&b, `

---

## Your situation

You are agent-nexus, an automated reviewer, answering a conversation on pull request #%d of %s on %s.
%s You may call several tools in one turn.
When you are done investigating, reply in plain text without calling any tool. You will then be asked for your final answer as JSON.
`, pr.Number, repo, forgeName, tools)
	if guidance != "" {
		fmt.Fprintf(&b, "\n## Repository guidance\n\nThe repository's %s, read from the base branch, says:\n\n<repository-guidance>\n%s\n</repository-guidance>\n", guidanceName, truncate(guidance, maxGuidance))
	}
	return b.String()
}

func discussionUserPrompt(repo forge.Repo, forgeName string, pr forge.PR, ws *workspace, a ask, login, history string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<pull-request>\nRepository: %s (%s)\nPull request: #%d: %s\nAuthor: %s\nState: %s\nBase: %s @ %s; head: %s\n</pull-request>\n\n## Changed files\n\n",
		repo, forgeName, pr.Number, pr.Title, pr.Author, prState(pr), pr.BaseRef, short(ws.baseSHA), short(ws.headSHA))
	for _, f := range ws.files {
		fmt.Fprintf(&b, "- %s %s (+%d -%d)\n", f.Status, displayPath(f), f.Additions, f.Deletions)
	}
	b.WriteString("\n## The conversation\n\n")
	if a.inline {
		root := a.thread[0]
		fmt.Fprintf(&b, "%s, in a thread you take part in.\n", threadTitle(a.thread))
		if d, ok := ws.fileDiff(root.Path); ok {
			fmt.Fprintf(&b, "\nThe pull request's diff of that file (new-file line numbers in the left column):\n\n<file path=%q>\n%s</file>\n", root.Path, truncate(d, maxNoteChars*4))
		} else {
			b.WriteString("\nThat file is not changed by the pull request any more.\n")
		}
	} else {
		fmt.Fprintf(&b, "%s mentioned you in a pull request %s.\n", a.covered[0].User, a.covered[0].Kind)
	}
	b.WriteString("\n<conversation>\n")
	for _, n := range a.thread {
		writeNote(&b, n, login)
	}
	b.WriteString("</conversation>\n")
	if !a.inline && history != "" {
		fmt.Fprintf(&b, "\nFor background, the rest of the discussion on this pull request:\n\n<discussion>\n%s</discussion>\n", history)
	}
	fmt.Fprintf(&b, "\nAnswer the latest message(s) from %s.\n", strings.Join(a.askers(), ", "))
	return b.String()
}

func prState(pr forge.PR) string {
	switch {
	case pr.Merged:
		return "merged"
	case pr.State != "open":
		return "closed without merging"
	case pr.Draft:
		return "open, draft"
	default:
		return "open"
	}
}

func discussionFinalInstruction(turnLimitReached, settling bool) string {
	var b strings.Builder
	if turnLimitReached {
		b.WriteString("You have reached the investigation limit. ")
	}
	if settling {
		b.WriteString(`Now write your answer as JSON: {"reply": "<markdown>", "resolved": [<numbers>]}.`)
	} else {
		b.WriteString(`Now write your answer as JSON: {"reply": "<markdown>"}.`)
	}
	b.WriteString(`
- Answer what was asked, in a few sentences; quote the code you rely on.
- If they are right, say so plainly and withdraw the finding; if not, explain why with the evidence.`)
	if settling {
		b.WriteString(`
- "resolved" lists the numbers of the open findings of your latest review that this conversation settles: you withdrew them, the head commit fixes them, or their author deferred them and you agree they need not block this pull request. Leave out a finding you keep, or one this conversation does not settle; use [] when none.`)
	}
	return b.String()
}
