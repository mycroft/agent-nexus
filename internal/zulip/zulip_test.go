package zulip_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/zulip"
	"github.com/mycroft/agent-nexus/internal/zulip/zuliptest"
)

var (
	alice = zulip.User{ID: 7, FullName: "Alice"}
	bob   = zulip.User{ID: 8, FullName: "Bob"}
)

func TestConversationOf(t *testing.T) {
	conv, err := zulip.ConversationOf(zuliptest.StreamMessage(3, "ops", "Helm Diff", alice, "hi"), zuliptest.Bot.Email)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Direct() || conv.StreamID != 3 || conv.Stream != "ops" || conv.Key() != "stream:3:helm diff" || conv.String() != "#ops > Helm Diff" {
		t.Errorf("channel conversation = %+v, key %q, %q", conv, conv.Key(), conv)
	}

	// A group direct message: the bot is left out, the others sorted.
	recipients, _ := json.Marshal([]map[string]any{
		{"id": bob.ID, "email": "bob@example.com", "full_name": bob.FullName},
		{"id": zuliptest.Bot.ID, "email": "Agent-Nexus-Bot@zulip.example.com", "full_name": zuliptest.Bot.FullName},
		{"id": alice.ID, "email": "alice@example.com", "full_name": alice.FullName},
	})
	conv, err = zulip.ConversationOf(zulip.Message{Type: "private", DisplayRecipient: recipients}, zuliptest.Bot.Email)
	if err != nil {
		t.Fatal(err)
	}
	if !conv.Direct() || len(conv.Users) != 2 || conv.Key() != "dm:7,8" || conv.String() != "direct messages with Alice, Bob" {
		t.Errorf("direct conversation = %+v, key %q, %q", conv, conv.Key(), conv)
	}

	for name, m := range map[string]zulip.Message{
		"unknown type":      {Type: "other"},
		"bad channel":       {Type: "stream", DisplayRecipient: json.RawMessage(`[]`), StreamID: 3},
		"no channel ID":     {Type: "stream", DisplayRecipient: json.RawMessage(`"ops"`)},
		"only the bot":      {Type: "private", DisplayRecipient: json.RawMessage(`[{"id":99,"email":"agent-nexus-bot@zulip.example.com"}]`)},
		"bad direct recips": {Type: "private", DisplayRecipient: json.RawMessage(`"ops"`)},
	} {
		if _, err := zulip.ConversationOf(m, zuliptest.Bot.Email); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestClient(t *testing.T) {
	ctx := context.Background()
	fake := zuliptest.New(t)
	c := fake.Client()

	me, err := c.Me(ctx)
	if err != nil || me != zuliptest.Bot {
		t.Fatalf("Me = %+v, %v", me, err)
	}

	q := fake.Add(zuliptest.StreamMessage(3, "ops", "helm", alice, "@**agent-nexus** what?"))
	fake.Add(zuliptest.StreamMessage(3, "ops", "other topic", bob, "elsewhere"))
	dm := fake.Add(zuliptest.DirectMessage(bob, "hello"))
	topic, _ := zulip.ConversationOf(q, me.Email)
	direct, _ := zulip.ConversationOf(dm, me.Email)

	history, err := c.History(ctx, topic, 10)
	if err != nil || len(history) != 1 || history[0].ID != q.ID || history[0].Content != q.Content {
		t.Fatalf("History(topic) = %+v, %v", history, err)
	}
	if history, err = c.History(ctx, direct, 10); err != nil || len(history) != 1 || history[0].ID != dm.ID {
		t.Fatalf("History(direct) = %+v, %v", history, err)
	}

	if _, err := c.Send(ctx, topic, "answer"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(ctx, direct, "private answer"); err != nil {
		t.Fatal(err)
	}
	sent := fake.Sent()
	if len(sent) != 2 || sent[0].StreamID != 3 || sent[0].Subject != "helm" || sent[0].Content != "answer" || sent[1].Type != "private" {
		t.Errorf("sent = %+v", sent)
	}

	for range 2 {
		if err := c.React(ctx, q.ID, "eyes"); err != nil {
			t.Fatalf("React: %v", err)
		}
	}
	if m := fake.Message(q.ID); !m.ReactedBy(me.ID, "eyes") || len(m.Reactions) != 1 {
		t.Errorf("reactions = %+v", m.Reactions)
	}
	if err := c.React(ctx, 12345, "eyes"); err == nil {
		t.Error("React on a missing message succeeded")
	}

	if _, err := zulip.New(fake.URL(), me.Email, "wrong", httpclient.WithRetry(0, 0, 0)).Me(ctx); err == nil {
		t.Error("Me with a wrong API key succeeded")
	}
}
