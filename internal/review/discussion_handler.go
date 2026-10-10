package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mycroft/agent-nexus/internal/agent"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
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

// DiscussionHandler returns the handler of DiscussionKind jobs.
func (r *Reviewer) DiscussionHandler() jobs.Handler { return discussionHandler{r} }

type discussionHandler struct{ r *Reviewer }

// Handle answers the pending replies and mentions of a pull request. It
// reads the whole discussion rather than the comment that triggered it:
// the event may not identify the comment, and a missed event is caught up
// by the next one.
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
	pr, err := f.GetPR(ctx, repoID, req.PR)
	if err != nil {
		return jobs.Result{}, err
	}
	if pr.State != "open" {
		return jobs.Result{Status: jobs.Skipped, Data: map[string]any{"outcome": SkippedClosed}}, nil
	}
	login, err := f.BotLogin(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	notes, err := f.ListNotes(ctx, repoID, req.PR)
	if err != nil {
		return jobs.Result{}, err
	}
	asks := pendingAsks(notes, login)
	if len(asks) == 0 {
		return jobs.Result{Status: jobs.Skipped, Data: map[string]any{"outcome": "nothing_to_answer"}}, nil
	}

	ws, _, err := r.prepare(ctx, f, repoID, pr, pr.HeadSHA)
	if err != nil {
		return jobs.Result{}, err
	}
	if ws == nil {
		return jobs.Result{}, errors.New("the pull request head moved while fetching it")
	}
	record := func(st agent.Step) {
		run.Record(ctx, jobs.Step{
			Role: st.Role, ToolName: st.ToolName, Content: st.Content,
			PromptTokens: st.PromptTokens, CompletionTokens: st.CompletionTokens,
		})
	}
	history := renderHistory(notes, login)
	answered := 0
	for _, a := range asks[:min(len(asks), maxAnswersPerJob)] {
		text, err := r.answer(ctx, f.Name(), repoID, pr, ws, a, login, history, record)
		if err != nil {
			return jobs.Result{}, err
		}
		posted, err := r.postAnswer(ctx, f, repoID, pr.Number, a, login, text, job.ID)
		if err != nil {
			return jobs.Result{}, err
		}
		if posted {
			answered++
		}
	}
	return jobs.Result{Status: jobs.Succeeded, Data: map[string]any{
		"outcome": "answered", "answered": answered, "pending": len(asks) - answered,
	}}, nil
}

// answer runs the agent on one conversation and returns its reply.
func (r *Reviewer) answer(ctx context.Context, forgeName string, repoID forge.Repo, pr forge.PR, ws *workspace, a ask, login, history string, record func(agent.Step)) (string, error) {
	runner := &agent.Runner{
		LLM:           r.LLM,
		ContextTokens: r.ContextTokens,
		MaxTurns:      min(r.MaxTurns, discussionMaxTurns),
		MaxToolOutput: maxToolOutput,
		Record:        record,
		Logger:        r.Logger,
	}
	system := discussionSystemPrompt(r.DiscussionSkill.Body, repoID, forgeName, pr, ws.guidanceName, ws.guidance)
	user := discussionUserPrompt(repoID, forgeName, pr, ws, a, login, history)
	session, err := runner.Run(ctx, system, user, ws.tools())
	if err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	var reply string
	err = session.Finish(ctx, discussionFinalInstruction(session.TurnLimitReached),
		llm.JSONSchema{Name: "pr_discussion_reply", Schema: replySchema},
		func(raw []byte) error {
			var out struct {
				Reply string `json:"reply"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}
			if reply = strings.TrimSpace(out.Reply); reply == "" {
				return errors.New("reply is empty")
			}
			return nil
		})
	if err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	return reply, nil
}

// postAnswer posts the reply in the thread, or as a pull request comment
// for a mention, unless another job answered meanwhile. It reports
// whether it posted.
func (r *Reviewer) postAnswer(ctx context.Context, f forge.Forge, repoID forge.Repo, number int, a ask, login, text string, jobID int64) (bool, error) {
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

	body := replyBody(a, text, jobID)
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
		return true, f.CreateReview(ctx, repoID, number, forge.NewReview{Event: forge.EventComment, Body: body})
	}
	return err == nil, err
}

// replyBody formats an answer: a quote of the note for a mention (it is
// posted apart from it), the text, a footer, and one marker per covered
// note.
func replyBody(a ask, text string, jobID int64) string {
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
	return b.String()
}

func discussionSystemPrompt(skillBody string, repo forge.Repo, forgeName string, pr forge.PR, guidanceName, guidance string) string {
	var b strings.Builder
	b.WriteString(skillBody)
	fmt.Fprintf(&b, `

---

## Your situation

You are agent-nexus, an automated reviewer, answering a conversation on pull request #%d of %s on %s.
You have read-only tools over the repository at the pull request's head commit: read_file, list_dir, grep, git_log and get_file_diff. You may call several tools in one turn.
When you are done investigating, reply in plain text without calling any tool. You will then be asked for your final answer as JSON.
`, pr.Number, repo, forgeName)
	if guidance != "" {
		fmt.Fprintf(&b, "\n## Repository guidance\n\nThe repository's %s, read from the base branch, says:\n\n<repository-guidance>\n%s\n</repository-guidance>\n", guidanceName, truncate(guidance, maxGuidance))
	}
	return b.String()
}

func discussionUserPrompt(repo forge.Repo, forgeName string, pr forge.PR, ws *workspace, a ask, login, history string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<pull-request>\nRepository: %s (%s)\nPull request: #%d: %s\nAuthor: %s\nBase: %s @ %s; head: %s\n</pull-request>\n\n## Changed files\n\n",
		repo, forgeName, pr.Number, pr.Title, pr.Author, pr.BaseRef, short(ws.baseSHA), short(ws.headSHA))
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

func discussionFinalInstruction(turnLimitReached bool) string {
	var b strings.Builder
	if turnLimitReached {
		b.WriteString("You have reached the investigation limit. ")
	}
	b.WriteString(`Now write your answer as JSON: {"reply": "<markdown>"}.
- Answer what was asked, in a few sentences; quote the code you rely on.
- If they are right, say so plainly and withdraw the finding; if not, explain why with the evidence.`)
	return b.String()
}
