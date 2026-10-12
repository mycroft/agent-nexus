package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mycroft/agent-nexus/internal/agent"
	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/llm"
	"github.com/mycroft/agent-nexus/internal/mcp"
	"github.com/mycroft/agent-nexus/internal/skill"
	"github.com/mycroft/agent-nexus/internal/zulip"
)

// ChatKind is the job kind answering the questions asked on Zulip.
const ChatKind = "chat_question"

const (
	// chatHistorySize is how many of a conversation's latest messages a
	// pass reads.
	chatHistorySize = 50
	// chatMaxTurns bounds the investigation behind one answer: more than
	// for a pull request discussion, as a question may be about any code.
	chatMaxTurns = 16
	// maxChatReply keeps an answer under Zulip's default message length.
	maxChatReply = 9000
	// ChatSeenEmoji is the reaction acknowledging a question when its job
	// is queued.
	ChatSeenEmoji = "eyes"
	// chatAnsweredEmoji marks the questions an answer covers. Only the
	// bot's own reaction counts.
	chatAnsweredEmoji = "check"
)

// ChatRequest identifies the conversation whose questions to answer. It
// is the job payload.
type ChatRequest struct {
	Conversation zulip.Conversation `json:"conversation"`
	// Label names the conversation in the jobs UI.
	Label string `json:"label"`
}

// ChatJob builds the job answering the conversation of a message. As for
// pull request discussions, a newer message replaces a queued job of the
// conversation and lets a running one finish. Zulip's retries of a
// message are the same job.
func ChatJob(conv zulip.Conversation, messageID int64, asker string) jobs.NewJob {
	return jobs.NewJob{
		Kind:        ChatKind,
		GroupKey:    "zulip:" + conv.Key(),
		DedupeKey:   fmt.Sprintf("zulip:message:%d", messageID),
		KeepRunning: true,
		APIKeyName:  "zulip:" + asker,
		Payload:     ChatRequest{Conversation: conv, Label: conv.String()},
	}
}

// Chat answers the questions asked to the bot on Zulip, with the
// Reviewer's tools and forges. A conversation naming a repository or a pull
// request is answered from it; others get a bare workspace, without a
// repository. It never writes to a forge.
type Chat struct {
	Reviewer *Reviewer
	Zulip    *zulip.Client
	// Forges are the configured forges, in order: owner/name names a
	// repository of the first one.
	Forges []forge.Forge
	// AllowedOwners are the owners whose repositories may be discussed.
	AllowedOwners []string
	Skill         skill.Skill
}

// Handle answers the conversation's pending questions in one message, then
// marks them answered. Like discussions, it reads the conversation rather
// than the message that triggered it, so a missed event is caught up by
// the next one.
func (c *Chat) Handle(ctx context.Context, job *jobs.Job, run *jobs.Run) (jobs.Result, error) {
	var req ChatRequest
	if err := json.Unmarshal(job.Payload, &req); err != nil {
		return jobs.Result{}, fmt.Errorf("decode chat request: %w", err)
	}
	conv := req.Conversation
	me, err := c.Zulip.Me(ctx)
	if err != nil {
		return jobs.Result{}, err
	}
	history, err := c.Zulip.History(ctx, conv, chatHistorySize)
	if err != nil {
		return jobs.Result{}, err
	}
	asks := pendingChatAsks(history, me, conv.Direct())
	if len(asks) == 0 {
		return jobs.Result{Status: jobs.Skipped, Data: map[string]any{"outcome": "nothing_to_answer"}}, nil
	}

	data := map[string]any{"outcome": "answered", "answered": len(asks)}
	var t *chatTarget
	if found, ok := c.target(history, asks, me, conv); ok {
		t = &found
		data["repo"] = t.String()
	}
	record := func(st agent.Step) {
		run.Record(ctx, jobs.Step{
			Role: st.Role, ToolName: st.ToolName, Content: st.Content,
			PromptTokens: st.PromptTokens, CompletionTokens: st.CompletionTokens,
		})
	}
	text, err := c.answer(ctx, t, conv, history, asks, me, record)
	if err != nil {
		// A cancelled job was interrupted by a shutdown, and runs again on
		// restart: it did not fail.
		if !errors.Is(ctx.Err(), context.Canceled) {
			c.reportFailure(ctx, conv, asks, job.ID)
		}
		return jobs.Result{}, err
	}
	if _, err := c.Zulip.Send(ctx, conv, chatReplyBody(asks, text, job.ID, conv.Direct())); err != nil {
		return jobs.Result{}, err
	}
	// A question left unmarked is answered again by the next pass.
	for _, a := range asks {
		if err := c.Zulip.React(ctx, a.ID, chatAnsweredEmoji); err != nil {
			c.Reviewer.logger().WarnContext(ctx, "mark question answered", "message", a.ID, "error", err)
		}
	}
	return jobs.Result{Status: jobs.Succeeded, Data: data}, nil
}

// reportFailure tells the askers their questions were not answered. They
// stay unmarked, so the next question in the conversation retries them.
func (c *Chat) reportFailure(ctx context.Context, conv zulip.Conversation, asks []zulip.Message, jobID int64) {
	// The job's context may be what ran out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	text := "I could not answer this: the job failed. Ask me again to retry."
	if _, err := c.Zulip.Send(ctx, conv, chatReplyBody(asks, text, jobID, conv.Direct())); err != nil {
		c.Reviewer.logger().WarnContext(ctx, "report a failed answer", "error", err)
	}
}

// zulipMention matches a mention of the bot in raw Zulip Markdown,
// "@**Name**" or "@**Name|ID**", but not a silent "@_**Name**".
func zulipMention(me zulip.Me) *regexp.Regexp {
	id := strconv.FormatInt(me.ID, 10)
	return regexp.MustCompile(`(?i)@\*\*(` + regexp.QuoteMeta(me.FullName) + `(\|` + id + `)?|[^*\n]*\|` + id + `)\*\*`)
}

// pendingChatAsks returns the questions waiting for an answer, oldest
// first: the messages of other users the bot has not marked answered that
// mention it, or in a direct message conversation all of them.
func pendingChatAsks(history []zulip.Message, me zulip.Me, direct bool) []zulip.Message {
	mention := zulipMention(me)
	var asks []zulip.Message
	for _, m := range history {
		if m.SenderID == me.ID || m.ReactedBy(me.ID, chatAnsweredEmoji) {
			continue
		}
		if direct || mention.MatchString(m.Content) {
			asks = append(asks, m)
		}
	}
	return asks
}

// chatTarget is what a question is about: a repository, and maybe one of
// its pull requests.
type chatTarget struct {
	forge forge.Forge
	repo  forge.Repo
	pr    int
}

func (t chatTarget) String() string {
	if t.pr > 0 {
		return fmt.Sprintf("%s#%d", t.repo, t.pr)
	}
	return t.repo.String()
}

// target finds what the conversation is about: the first repository or
// pull request named by the pending questions, newest first, then by the
// other users' earlier messages, the topic and the channel name.
func (c *Chat) target(history, asks []zulip.Message, me zulip.Me, conv zulip.Conversation) (chatTarget, bool) {
	var texts []string
	for _, a := range slices.Backward(asks) {
		texts = append(texts, a.Content)
	}
	for _, m := range slices.Backward(history) {
		if m.SenderID != me.ID {
			texts = append(texts, m.Content)
		}
	}
	texts = append(texts, conv.Topic, conv.Stream)
	for _, text := range texts {
		if t, ok := c.parseTarget(text); ok {
			return t, true
		}
	}
	return chatTarget{}, false
}

// shorthandPattern matches "owner/name" and "owner/name#123".
var shorthandPattern = regexp.MustCompile(`([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)(?:#(\d+))?`)

// parseTarget returns the first reference to a repository of an allowed
// owner in text: a link to a forge (a pull request link names the pull
// request), or "owner/name[#number]" on the first forge.
func (c *Chat) parseTarget(text string) (chatTarget, bool) {
	var best chatTarget
	bestAt := -1
	consider := func(at int, f forge.Forge, owner, name, number string) {
		if bestAt >= 0 && at >= bestAt {
			return
		}
		name = strings.TrimSuffix(strings.TrimRight(name, "."), ".git")
		repo, err := forge.ParseRepo(owner + "/" + name)
		if err != nil || !slices.ContainsFunc(c.AllowedOwners, func(o string) bool { return strings.EqualFold(o, repo.Owner) }) {
			return
		}
		pr, _ := strconv.Atoi(number)
		best, bestAt = chatTarget{forge: f, repo: repo, pr: pr}, at
	}
	group := func(m []int, i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return text[m[2*i]:m[2*i+1]]
	}
	for _, f := range c.Forges {
		pattern, err := forgeLinkPattern(f)
		if err != nil {
			continue
		}
		for _, m := range pattern.FindAllStringSubmatchIndex(text, -1) {
			consider(m[0], f, group(m, 1), group(m, 2), group(m, 3))
		}
	}
	if len(c.Forges) > 0 {
		for _, m := range shorthandPattern.FindAllStringSubmatchIndex(text, -1) {
			// The rest of a link, or of a path.
			if m[0] > 0 && strings.ContainsRune("/:@", rune(text[m[0]-1])) {
				continue
			}
			consider(m[0], c.Forges[0], group(m, 1), group(m, 2), group(m, 3))
		}
	}
	return best, bestAt >= 0
}

// forgeLinkPattern matches a link to a repository of f, or to one of its
// pull requests (Gitea's /pulls/N, GitHub's /pull/N). The host must start
// there: sub.git.example.com is another host.
func forgeLinkPattern(f forge.Forge) (*regexp.Regexp, error) {
	u, err := url.Parse(f.ServerURL())
	if err != nil {
		return nil, fmt.Errorf("forge URL: %w", err)
	}
	root := strings.Trim(u.Host+u.Path, "/")
	return regexp.Compile(`(?i)(?:^|[^A-Za-z0-9_.-])(?:https?://)?` + regexp.QuoteMeta(root) + `/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)(?:/pulls?/(\d+))?`)
}

// namingHint tells the model how a question names a repository, for it to
// ask which one a question is about.
func (c *Chat) namingHint() string {
	s := "They can name it as `owner/name`, or `owner/name#<number>` for a pull request, or paste its link; a topic named after it works too."
	if len(c.Forges) > 1 {
		s += fmt.Sprintf(" `owner/name` is on %s: a repository of another forge needs a link.", c.Forges[0].Name())
	}
	return s + fmt.Sprintf(" You can read the repositories of %s.", strings.Join(c.AllowedOwners, ", "))
}

// chatSubject is a chat target ready for the agent: its workspace, and
// for a pull request, the pull request and its discussion.
type chatSubject struct {
	ws         *workspace
	pr         *forge.PR
	discussion string
}

// prepareTarget fetches what t names, or returns a bare workspace when t
// is nil. When the forge does not know t, the returned string is the
// answer saying so.
func (c *Chat) prepareTarget(ctx context.Context, t *chatTarget) (chatSubject, string, error) {
	r := c.Reviewer
	if t == nil {
		return chatSubject{ws: r.bareWorkspace()}, "", nil
	}
	if t.pr == 0 {
		ws, err := r.prepareBranch(ctx, t.forge, t.repo)
		if errors.Is(err, forge.ErrNotFound) {
			return chatSubject{}, fmt.Sprintf("I cannot find %s on %s, or I may not read it.", t.repo, t.forge.Name()), nil
		}
		return chatSubject{ws: ws}, "", err
	}
	pr, err := t.forge.GetPR(ctx, t.repo, t.pr)
	if errors.Is(err, forge.ErrNotFound) {
		return chatSubject{}, fmt.Sprintf("I cannot find pull request %s on %s.", t, t.forge.Name()), nil
	}
	if err != nil {
		return chatSubject{}, "", err
	}
	ws, _, err := r.prepare(ctx, t.forge, t.repo, pr, pr.HeadSHA)
	if err == nil && ws == nil {
		err = errors.New("the pull request head moved while fetching it")
	}
	if err != nil {
		return chatSubject{}, "", err
	}
	return chatSubject{ws: ws, pr: &pr, discussion: r.prDiscussion(ctx, *t)}, "", nil
}

// answer runs the agent on the conversation and returns its reply. A
// repository or pull request it cannot read is answered as such; a nil t
// is no repository.
func (c *Chat) answer(ctx context.Context, t *chatTarget, conv zulip.Conversation, history, asks []zulip.Message, me zulip.Me, record func(agent.Step)) (string, error) {
	r := c.Reviewer
	s, missing, err := c.prepareTarget(ctx, t)
	if err != nil || missing != "" {
		return missing, err
	}
	ws := s.ws

	runner := &agent.Runner{
		LLM:           r.LLM,
		ContextTokens: r.ContextTokens,
		MaxTurns:      min(r.MaxTurns, chatMaxTurns),
		MaxToolOutput: maxToolOutput,
		Record:        record,
		Logger:        r.Logger,
	}
	mcpTools, mcpSentence, closeMCP := r.openMCP(ctx, mcp.JobChat)
	defer closeMCP()
	toolset := append(ws.tools(), mcpTools...)
	system := c.systemPrompt(t, ws, withMCP(ws.toolsSentence(), mcpSentence), len(toolset) > 0)
	user := chatUserPrompt(t, s.pr, ws, s.discussion, conv, history, asks, me)
	session, err := runner.Run(ctx, system, user, toolset)
	if err != nil {
		return "", fmt.Errorf("agent: %w", err)
	}
	var reply string
	err = session.Finish(ctx, chatFinalInstruction(session.TurnLimitReached, t != nil),
		llm.JSONSchema{Name: "chat_reply", Schema: replySchema},
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

// prDiscussion renders the pull request's discussion, or "" when it has
// none. Failing to read it only loses context, so it is logged.
func (r *Reviewer) prDiscussion(ctx context.Context, t chatTarget) string {
	login, err := t.forge.BotLogin(ctx)
	if err == nil {
		var notes []forge.Note
		if notes, err = t.forge.ListNotes(ctx, t.repo, t.pr); err == nil {
			return renderHistory(notes, login)
		}
	}
	r.logger().WarnContext(ctx, "pull request discussion unavailable", "repo", t.repo.String(), "pr", t.pr, "error", err)
	return ""
}

// systemPrompt is the skill and the situation: what the conversation is
// about (t, or nothing when nil) and the tools the agent has, which
// toolsSentence describes.
func (c *Chat) systemPrompt(t *chatTarget, ws *workspace, toolsSentence string, hasTools bool) string {
	var b strings.Builder
	b.WriteString(c.Skill.Body)
	b.WriteString("\n\n---\n\n## Your situation\n\n")
	switch {
	case t == nil:
		fmt.Fprintf(&b, "You are agent-nexus, answering a question asked in Zulip, the team chat. The conversation names no repository or pull request, so you have no code to read. If a question is about code, a repository or a pull request, ask which one instead of guessing. %s\n", c.namingHint())
	case t.pr > 0:
		fmt.Fprintf(&b, "You are agent-nexus, answering a question asked in Zulip, the team chat, about pull request #%d of %s on %s.\n", t.pr, t.repo, t.forge.Name())
	default:
		fmt.Fprintf(&b, "You are agent-nexus, answering a question asked in Zulip, the team chat, about %s on %s.\n", t.repo, t.forge.Name())
	}
	if hasTools {
		fmt.Fprintf(&b, "%s You may call several tools in one turn.\nWhen you are done investigating, reply in plain text without calling any tool.", toolsSentence)
	} else {
		b.WriteString("You have no tools: reply in plain text.")
	}
	b.WriteString(" You will then be asked for your final answer as JSON.\n")
	if ws.guidance != "" {
		fmt.Fprintf(&b, "\n## Repository guidance\n\nThe repository's %s, read from %s, says:\n\n<repository-guidance>\n%s\n</repository-guidance>\n",
			ws.guidanceName, guidanceSource(ws), truncate(ws.guidance, maxGuidance))
	}
	return b.String()
}

func guidanceSource(ws *workspace) string {
	if ws.branch != "" {
		return "the " + ws.branch + " branch"
	}
	return "the base branch"
}

func chatUserPrompt(t *chatTarget, pr *forge.PR, ws *workspace, background string, conv zulip.Conversation, history, asks []zulip.Message, me zulip.Me) string {
	var b strings.Builder
	switch {
	case pr != nil:
		fmt.Fprintf(&b, "<pull-request>\nRepository: %s (%s)\nPull request: #%d: %s\nAuthor: %s\nState: %s\nBase: %s @ %s; head: %s\n",
			t.repo, t.forge.Name(), pr.Number, pr.Title, pr.Author, prState(*pr), pr.BaseRef, short(ws.baseSHA), short(ws.headSHA))
		if body := strings.TrimSpace(pr.Body); body != "" {
			fmt.Fprintf(&b, "Description:\n%s\n", truncate(body, maxNoteChars))
		}
		b.WriteString("</pull-request>\n\n## Changed files\n\n")
		for _, f := range ws.files {
			fmt.Fprintf(&b, "- %s %s (+%d -%d)\n", f.Status, displayPath(f), f.Additions, f.Deletions)
		}
		if background != "" {
			fmt.Fprintf(&b, "\nFor background, the discussion on the pull request, your reviews included:\n\n<discussion>\n%s</discussion>\n", background)
		}
	case t != nil:
		fmt.Fprintf(&b, "<repository>\nRepository: %s (%s)\nBranch: %s @ %s\n</repository>\n", t.repo, t.forge.Name(), ws.branch, short(ws.headSHA))
	}

	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString("## The conversation\n\n")
	if conv.Direct() {
		b.WriteString("In direct messages with you.\n")
	} else {
		fmt.Fprintf(&b, "In the channel #%s, topic %q.\n", conv.Stream, conv.Topic)
	}
	pending := map[int64]bool{}
	var askers []string
	for _, a := range asks {
		pending[a.ID] = true
		if !slices.Contains(askers, a.SenderFullName) {
			askers = append(askers, a.SenderFullName)
		}
	}
	fmt.Fprintf(&b, "\n<conversation>\n%s</conversation>\n", chatConversation(history, me, pending))
	fmt.Fprintf(&b, "\nAnswer the messages marked \"to answer\", from %s.\n", strings.Join(askers, ", "))
	return b.String()
}

var (
	// zulipMentionSyntax matches a user mention in raw Zulip Markdown.
	zulipMentionSyntax = regexp.MustCompile(`@_?\*\*([^*|\n]+)(?:\|\d+)?\*\*`)
	// chatFooterPattern matches the footer of the bot's answers.
	chatFooterPattern = regexp.MustCompile(`\n*_agent-nexus · job \d+_\s*$`)
)

// chatConversation renders the latest messages that fit maxHistoryChars,
// oldest first, marking the bot's and those to answer.
func chatConversation(history []zulip.Message, me zulip.Me, pending map[int64]bool) string {
	var entries []string
	size := 0
	for _, m := range slices.Backward(history) {
		who := m.SenderFullName
		switch {
		case m.SenderID == me.ID:
			who += " (you)"
		case pending[m.ID]:
			who += " (to answer)"
		}
		content := chatFooterPattern.ReplaceAllString(m.Content, "")
		content = zulipMentionSyntax.ReplaceAllString(content, "@$1")
		entry := fmt.Sprintf("**%s**:\n%s\n\n", who, truncate(strings.TrimSpace(content), maxNoteChars))
		if size+len(entry) > maxHistoryChars && len(entries) > 0 {
			break
		}
		size += len(entry)
		entries = append(entries, entry)
	}
	slices.Reverse(entries)
	return strings.Join(entries, "")
}

// chatFinalInstruction asks for the answer. Without a repository (code is
// false), there is no code to cite.
func chatFinalInstruction(turnLimitReached, code bool) string {
	var b strings.Builder
	if turnLimitReached {
		b.WriteString("You have reached the investigation limit. ")
	}
	b.WriteString(`Now write your answer as JSON: {"reply": "<markdown>"}.` + "\n")
	if code {
		b.WriteString("- Answer what was asked; quote the code you rely on and cite path:line.\n")
	} else {
		b.WriteString("- Answer what was asked. If it is about code, a repository or a pull request, ask which one instead.\n")
	}
	b.WriteString("- Zulip Markdown: no headings and no HTML; use lists, `code` and fenced code blocks.")
	return b.String()
}

// chatReplyBody formats an answer. In a channel it mentions the askers
// first, so they are notified; a footer names the job.
func chatReplyBody(asks []zulip.Message, text string, jobID int64, direct bool) string {
	var b strings.Builder
	if !direct {
		var seen []int64
		for _, a := range asks {
			if !slices.Contains(seen, a.SenderID) {
				seen = append(seen, a.SenderID)
				fmt.Fprintf(&b, "@**%s|%d** ", a.SenderFullName, a.SenderID)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString(truncate(strings.TrimSpace(text), maxChatReply))
	fmt.Fprintf(&b, "\n\n_agent-nexus · job %d_", jobID)
	return b.String()
}
