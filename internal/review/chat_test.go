package review

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/forge"
	"github.com/mycroft/agent-nexus/internal/forge/forgetest"
	"github.com/mycroft/agent-nexus/internal/jobs"
	"github.com/mycroft/agent-nexus/internal/skill"
	"github.com/mycroft/agent-nexus/internal/zulip"
	"github.com/mycroft/agent-nexus/internal/zulip/zuliptest"
	"github.com/mycroft/agent-nexus/skills"
)

var (
	alice = zulip.User{ID: 7, FullName: "Alice"}
	bob   = zulip.User{ID: 8, FullName: "Bob"}
)

func TestParseTarget(t *testing.T) {
	gitea := forgetest.New("gitea", "https://git.example.com")
	github := forgetest.New("github", "https://github.com")
	c := &Chat{Forges: []forge.Forge{gitea, github}, AllowedOwners: []string{"mycroft", "o"}}
	for _, tc := range []struct {
		text  string
		forge string
		want  string
	}{
		{"what does mycroft/k8s-home#1685 change?", "gitea", "mycroft/k8s-home#1685"},
		{"see https://git.example.com/mycroft/k8s-home/pulls/12", "gitea", "mycroft/k8s-home#12"},
		{"see <https://github.com/mycroft/agent-nexus/pull/3>", "github", "mycroft/agent-nexus#3"},
		{"in git.example.com/mycroft/charts/src/branch/main/x.yaml", "gitea", "mycroft/charts"},
		{"clone https://git.example.com/mycroft/charts.git", "gitea", "mycroft/charts"},
		{"Is it in Mycroft/k8s-home.", "gitea", "Mycroft/k8s-home"},
		{"mycroft/k8s-home/charts/apps/zulip.go", "gitea", "mycroft/k8s-home"},
		// The first reference wins; others' repositories are skipped.
		{"compare other/x with o/calc and mycroft/b", "gitea", "o/calc"},
		{"read https://git.example.com/other/x then o/calc#4", "gitea", "o/calc#4"},
		// Paths and links to other hosts are not references.
		{"charts/apps/zulip.go", "", ""},
		{"https://example.com/mycroft/k8s-home", "", ""},
		{"and/or", "", ""},
	} {
		got, ok := c.parseTarget(tc.text)
		switch {
		case tc.want == "" && ok:
			t.Errorf("parseTarget(%q) = %s, want none", tc.text, got)
		case tc.want != "" && (!ok || got.String() != tc.want || got.forge.Name() != tc.forge):
			t.Errorf("parseTarget(%q) = %v (%v), want %s on %s", tc.text, got, ok, tc.want, tc.forge)
		}
	}
}

func TestTargetOrder(t *testing.T) {
	c := &Chat{Forges: []forge.Forge{forgetest.New("gitea", "https://git.example.com")}, AllowedOwners: []string{"o"}}
	me := zuliptest.Bot
	conv := zulip.Conversation{StreamID: 3, Stream: "o/stream", Topic: "o/topic"}
	msg := func(sender zulip.User, content string) zulip.Message {
		return zulip.Message{SenderID: sender.ID, Content: content}
	}
	botMessage := zulip.Message{SenderID: me.ID, Content: "I also read o/bot"}
	for _, tc := range []struct {
		name    string
		history []zulip.Message
		asks    []int // indexes in history
		want    string
	}{
		{"latest ask first", []zulip.Message{msg(alice, "o/older"), msg(bob, "o/first-ask"), msg(alice, "o/second-ask")}, []int{1, 2}, "o/second-ask"},
		{"asks before later messages", []zulip.Message{msg(alice, "o/ask"), msg(bob, "o/chatter")}, []int{0}, "o/ask"},
		{"earlier messages, not the bot's", []zulip.Message{msg(alice, "o/earlier"), botMessage, msg(bob, "and?")}, []int{2}, "o/earlier"},
		{"then the topic", []zulip.Message{botMessage, msg(bob, "and?")}, []int{1}, "o/topic"},
	} {
		var asks []zulip.Message
		for _, i := range tc.asks {
			asks = append(asks, tc.history[i])
		}
		got, ok := c.target(tc.history, asks, me, conv)
		if !ok || got.String() != tc.want {
			t.Errorf("%s: target = %s (%v), want %s", tc.name, got, ok, tc.want)
		}
	}
	conv.Topic = "no reference"
	if got, ok := c.target(nil, nil, me, conv); !ok || got.String() != "o/stream" {
		t.Errorf("channel name fallback: target = %s (%v)", got, ok)
	}
}

func TestPendingChatAsks(t *testing.T) {
	me := zuliptest.Bot
	answered := []zulip.Reaction{{EmojiName: chatAnsweredEmoji, UserID: me.ID}}
	history := []zulip.Message{
		{ID: 1, SenderID: alice.ID, Content: "@**agent-nexus** first"},
		{ID: 2, SenderID: alice.ID, Content: "@**agent-nexus** answered", Reactions: answered},
		{ID: 3, SenderID: me.ID, Content: "@**Alice|7** an answer"},
		{ID: 4, SenderID: bob.ID, Content: "no mention"},
		{ID: 5, SenderID: bob.ID, Content: "@_**agent-nexus** silent mention"},
		{ID: 6, SenderID: bob.ID, Content: "@**Agent-Nexus|99** with ID"},
		{ID: 7, SenderID: bob.ID, Content: "@**old name|99** renamed"},
		{ID: 8, SenderID: bob.ID, Content: "@**agent-nexus-2** another bot"},
		// Someone else's check mark does not mark it answered.
		{ID: 9, SenderID: alice.ID, Content: "@**agent-nexus** again", Reactions: []zulip.Reaction{{EmojiName: chatAnsweredEmoji, UserID: bob.ID}}},
	}
	ids := func(asks []zulip.Message) []int64 {
		var out []int64
		for _, a := range asks {
			out = append(out, a.ID)
		}
		return out
	}
	if got := ids(pendingChatAsks(history, me, false)); !slices.Equal(got, []int64{1, 6, 7, 9}) {
		t.Errorf("channel asks = %v, want [1 6 7 9]", got)
	}
	if got := ids(pendingChatAsks(history, me, true)); !slices.Equal(got, []int64{1, 4, 5, 6, 7, 8, 9}) {
		t.Errorf("direct asks = %v, want every unanswered message of others", got)
	}
}

// chatFixture is a review fixture with a fake Zulip server: the forge
// holds repository o/calc and its pull request #4.
type chatFixture struct {
	*fixture
	zulip *zuliptest.Fake
	chat  *Chat
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	f := newFixture(t)
	s, err := skill.Load(skills.FS, "chat")
	if err != nil {
		t.Fatal(err)
	}
	z := zuliptest.New(t)
	return &chatFixture{fixture: f, zulip: z, chat: &Chat{
		Reviewer:      f.reviewer,
		Zulip:         z.Client(),
		Forges:        []forge.Forge{f.forge},
		AllowedOwners: []string{"o"},
		Skill:         s,
	}}
}

// ask posts a question in the #dev channel, topic "calc".
func (f *chatFixture) ask(sender zulip.User, content string) zulip.Message {
	return f.zulip.Add(zuliptest.StreamMessage(3, "dev", "calc", sender, content))
}

// run runs one chat job about m's conversation through the queue.
func (f *chatFixture) run(t *testing.T, m zulip.Message) jobs.Job {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := jobs.OpenStore(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	q := jobs.NewQueue(store, 1, 30*time.Minute, time.Hour, quiet)
	q.Register(ChatKind, f.chat)
	conv, err := zulip.ConversationOf(m, zuliptest.Bot.Email)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := q.Enqueue(ctx, ChatJob(conv, m.ID, m.SenderFullName))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- q.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := store.Get(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != jobs.Queued && got.Status != jobs.Running {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %s", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatAnswersAboutAPullRequest(t *testing.T) {
	f := newChatFixture(t)
	f.forge.AddNote(forge.Note{Kind: forge.NoteComment, User: "carol", Body: "Is Mod covered?"})
	f.ask(alice, "o/calc#4 adds a guard to Div.")
	q1 := f.ask(bob, "@**agent-nexus** does it guard Mod too?")
	q2 := f.ask(alice, "@**agent-nexus|99** and what does it return then?")
	replyScript(f.llm, "No: only `Div` is guarded (`calc/calc.go:5`).")

	job := f.run(t, q2)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"answered":2`) || !strings.Contains(string(job.Result), `"repo":"o/calc#4"`) {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	sent := f.zulip.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1: %+v", len(sent), sent)
	}
	want := "@**Bob|8** @**Alice|7** \nNo: only `Div` is guarded (`calc/calc.go:5`).\n\n_agent-nexus · job 1_"
	if sent[0].Content != want || sent[0].StreamID != 3 || sent[0].Subject != "calc" {
		t.Errorf("answer = %+v, want %q", sent[0], want)
	}
	for _, q := range []zulip.Message{q1, q2} {
		if !f.zulip.Message(q.ID).ReactedBy(zuliptest.Bot.ID, chatAnsweredEmoji) {
			t.Errorf("question %d not marked answered", q.ID)
		}
	}

	system := f.llm.requests[0].Messages[0].Content
	user := f.llm.requests[0].Messages[1].Content
	for _, want := range []string{"# Chat", "about pull request #4 of o/calc on gitea", "get_file_diff"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, system)
		}
	}
	for _, want := range []string{
		"Pull request: #4: Guard division", "## Changed files", "calc/calc.go",
		"Comment by carol", "Is Mod covered?",
		`In the channel #dev, topic "calc".`,
		"**Alice**:\no/calc#4 adds a guard to Div.",
		"**Bob (to answer)**:\n@agent-nexus does it guard Mod too?",
		"**Alice (to answer)**:\n@agent-nexus and what does it return then?",
		`Answer the messages marked "to answer", from Bob, Alice.`,
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, user)
		}
	}

	// Answered questions are not answered again.
	if job = f.run(t, q2); job.Status != jobs.Skipped || len(f.zulip.Sent()) != 1 {
		t.Errorf("second pass = %s %s, %d messages", job.Status, job.Result, len(f.zulip.Sent()))
	}

	// A follow-up keeps the topic's pull request, and sees the answer.
	q3 := f.ask(alice, "@**agent-nexus** thanks, and Div?")
	replyScript(f.llm, "Div returns 0.")
	if job = f.run(t, q3); job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"repo":"o/calc#4"`) {
		t.Fatalf("follow-up = %s %s %s", job.Status, job.Error, job.Result)
	}
	user = f.llm.requests[len(f.llm.requests)-1].Messages[1].Content
	if !strings.Contains(user, "**agent-nexus (you)**:\n@Bob @Alice \nNo: only `Div` is guarded") || strings.Contains(user, "_agent-nexus · job") {
		t.Errorf("follow-up prompt does not show the earlier answer without its footer:\n%s", user)
	}
}

func TestChatAnswersAboutARepository(t *testing.T) {
	f := newChatFixture(t)
	q := f.zulip.Add(zuliptest.DirectMessage(alice, "How is Div written in o/calc?"))
	replyScript(f.llm, "It returns a / b.")

	job := f.run(t, q)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), `"repo":"o/calc"`) {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	sent := f.zulip.Sent()
	// Direct answers mention no one.
	if len(sent) != 1 || sent[0].Type != "private" || sent[0].Content != "It returns a / b.\n\n_agent-nexus · job 1_" {
		t.Fatalf("sent = %+v", sent)
	}

	req := f.llm.requests[0]
	system, user := req.Messages[0].Content, req.Messages[1].Content
	for _, want := range []string{"about o/calc on gitea", "at the head of its main branch (" + short(f.remote.BaseSHA) + "): read_file, list_dir, grep and git_log.", "read from the main branch", "Errors must be returned"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt lacks %q:\n%s", want, system)
		}
	}
	for _, want := range []string{"Branch: main @ " + short(f.remote.BaseSHA), "In direct messages with you.", "**Alice (to answer)**:\nHow is Div written in o/calc?"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, user)
		}
	}
	var names []string
	for _, tool := range req.Tools {
		names = append(names, tool.Name)
	}
	if slices.Contains(names, "get_file_diff") || !slices.Contains(names, "read_file") {
		t.Errorf("tools = %v, want the repository tools without get_file_diff", names)
	}
	// The tools read the main branch, not the pull request.
	if !strings.Contains(f.llm.requests[1].Messages[3].Content, "return a / b") {
		t.Errorf("read_file did not read the main branch:\n%s", f.llm.requests[1].Messages[3].Content)
	}
}

func TestChatWithoutRepository(t *testing.T) {
	f := newChatFixture(t)
	q := f.ask(alice, "@**agent-nexus** what does charts/apps/zulip.go do in other/repo?")
	f.zulip.Add(zuliptest.StreamMessage(3, "dev", "elsewhere", bob, "o/calc"))

	job := f.run(t, q)
	if job.Status != jobs.Succeeded || !strings.Contains(string(job.Result), "no_repository") {
		t.Fatalf("job = %s %s %s", job.Status, job.Error, job.Result)
	}
	sent := f.zulip.Sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Content, "Which repository is this about?") || !strings.Contains(sent[0].Content, "repositories of o.") {
		t.Errorf("sent = %+v", sent)
	}
	if len(f.llm.requests) != 0 {
		t.Errorf("the model was called %d times", len(f.llm.requests))
	}
}

func TestChatPullRequestNotFound(t *testing.T) {
	f := newChatFixture(t)
	q := f.ask(alice, "@**agent-nexus** what about o/calc#5?")
	job := f.run(t, q)
	if job.Status != jobs.Succeeded {
		t.Fatalf("job = %s %s", job.Status, job.Error)
	}
	if sent := f.zulip.Sent(); len(sent) != 1 || !strings.Contains(sent[0].Content, "I cannot find pull request o/calc#5 on gitea.") {
		t.Errorf("sent = %+v", f.zulip.Sent())
	}
}

func TestChatReportsFailures(t *testing.T) {
	f := newChatFixture(t)
	q := f.ask(alice, "@**agent-nexus** what does o/calc#4 change?")
	// No scripted replies: the model call fails.
	job := f.run(t, q)
	if job.Status != jobs.Failed {
		t.Fatalf("job = %s %s", job.Status, job.Result)
	}
	sent := f.zulip.Sent()
	if len(sent) != 1 || !strings.Contains(sent[0].Content, "I could not answer this: the job failed.") {
		t.Errorf("sent = %+v", sent)
	}
	// It stays pending, for the next question to retry.
	if f.zulip.Message(q.ID).ReactedBy(zuliptest.Bot.ID, chatAnsweredEmoji) {
		t.Error("the failed question is marked answered")
	}
	if got := pendingChatAsks([]zulip.Message{f.zulip.Message(q.ID)}, zuliptest.Bot, false); len(got) != 1 {
		t.Errorf("pending asks = %v", got)
	}
}

func TestChatJob(t *testing.T) {
	conv := zulip.Conversation{StreamID: 3, Stream: "dev", Topic: "Calc"}
	nj := ChatJob(conv, 42, "Alice")
	req, _ := nj.Payload.(ChatRequest)
	if nj.Kind != ChatKind || nj.GroupKey != "zulip:stream:3:calc" || nj.DedupeKey != "zulip:message:42" || !nj.KeepRunning ||
		nj.APIKeyName != "zulip:Alice" || req.Label != "#dev > Calc" {
		t.Errorf("ChatJob = %+v", nj)
	}
}

func TestChatReplyBodyTruncates(t *testing.T) {
	body := chatReplyBody(nil, strings.Repeat("x", 2*maxChatReply), 1, true)
	if len(body) > maxChatReply+100 || !strings.Contains(body, "[truncated]") {
		t.Errorf("reply of %d bytes", len(body))
	}
}
