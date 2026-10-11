package review

import (
	"strings"
	"testing"
	"time"

	"github.com/mycroft/agent-nexus/internal/forge"
)

const bot = "agent-nexus"

func at(sec int) time.Time { return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC) }

func inline(id int64, user, thread, body string, sec int) forge.Note {
	return forge.Note{Kind: forge.NoteInline, ID: id, User: user, Body: body, Path: "calc/calc.go", Line: 13, Thread: thread, CreatedAt: at(sec)}
}

func comment(id int64, user, body string, sec int) forge.Note {
	return forge.Note{Kind: forge.NoteComment, ID: id, User: user, Body: body, CreatedAt: at(sec)}
}

func TestMentions(t *testing.T) {
	for body, want := range map[string]bool{
		"@agent-nexus why?":         true,
		"cc @Agent-Nexus.":          true,
		"(@agent-nexus)":            true,
		"mail agent-nexus@x.org":    false,
		"@agent-nexus-bot please":   false,
		"see foo/@agent-nexus":      true,
		"no mention of agent-nexus": false,
	} {
		if got := mentionPattern(bot).MatchString(body); got != want {
			t.Errorf("mentions(%q) = %v, want %v", body, got, want)
		}
	}
	if !mentionPattern("agent-nexus[bot]").MatchString("hey @agent-nexus") {
		t.Error("a GitHub App is mentioned without its [bot] suffix")
	}
}

func TestPendingAsks(t *testing.T) {
	notes := []forge.Note{
		// Thread a: the bot's finding, a reply, and an answered reply.
		inline(1, bot, "a", "**[blocking]** Mod panics", 1),
		inline(2, "alice", "a", "callers never pass 0", 2),
		// Thread b: a human thread; only the mention asks the bot.
		inline(3, "bob", "b", "should this be unsigned?", 3),
		inline(4, "alice", "b", "@agent-nexus what do you think?", 4),
		// A pull request comment mentioning the bot, already answered.
		comment(5, "alice", "@agent-nexus re-check please", 5),
		comment(6, bot, "done "+replyMarker(comment(5, "", "", 0)), 6),
		// A newer mention, not answered; and the bot mentioning itself.
		comment(7, "carol", "thanks @agent-nexus", 7),
		comment(8, bot, "@agent-nexus says hi", 8),
		// A review body mentioning the bot.
		{Kind: forge.NoteReview, ID: 9, User: "dave", Body: "LGTM, @agent-nexus?", CreatedAt: at(9)},
	}
	asks := pendingAsks(notes, bot)
	var got []string
	for _, a := range asks {
		var keys []string
		for _, n := range a.covered {
			keys = append(keys, n.Key())
		}
		got = append(got, strings.Join(keys, "+"))
	}
	want := "inline:2,inline:4,comment:7,review:9"
	if strings.Join(got, ",") != want {
		t.Fatalf("pending = %v, want %s", got, want)
	}
	if !asks[0].inline || len(asks[0].thread) != 2 || asks[2].inline {
		t.Errorf("asks = %+v", asks)
	}

	// Answering thread a's reply, and a second reply in it: one answer
	// covers what is still pending.
	notes = append(notes,
		inline(10, bot, "a", "You're right "+replyMarker(notes[1]), 10),
		inline(11, "alice", "a", "also see line 20", 11),
		inline(12, "bob", "a", "+1", 12),
	)
	asks = pendingAsks(notes, bot)
	if len(asks) != 4 || asks[3].covered[0].ID != 11 || len(asks[3].covered) != 2 {
		t.Errorf("after answer: %+v", asks)
	}
}

func TestRenderHistory(t *testing.T) {
	notes := []forge.Note{
		{Kind: forge.NoteReview, ID: 1, User: bot, Body: "## Summary\n\nOld review.\n\n---\n<sub>agent-nexus · reviewed abc · job 1</sub>\n" + marker("abc"), CreatedAt: at(1)},
		inline(2, bot, "a", "**[blocking]** Mod panics", 1),
		inline(3, "alice", "a", "callers never pass 0", 2),
		inline(4, "bob", "b", "a human-only thread", 3),
		comment(5, "carol", "please split this PR", 4),
		{Kind: forge.NoteReview, ID: 6, User: bot, Body: "## Summary\n\nNewest review.", CreatedAt: at(5)},
	}
	got := renderHistory(notes, bot)
	for _, want := range []string{
		"### Thread on calc/calc.go, line 13", "**agent-nexus (you)**:\n**[blocking]** Mod panics", "**alice**:\ncallers never pass 0",
		"### Comment by carol", "### Your latest review\n\n## Summary\n\nNewest review.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("history lacks %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"human-only thread", "Old review", "<sub>", "<!--"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("history contains %q:\n%s", unwanted, got)
		}
	}
	if renderHistory(nil, bot) != "" || historySection("") != "" {
		t.Error("no discussion must render nothing")
	}

	// Over budget, the oldest entries go first.
	var many []forge.Note
	for i := range 40 {
		many = append(many, comment(int64(100+i), "eve", strings.Repeat("x", 600)+string(rune('a'+i%26)), i))
	}
	got = renderHistory(many, bot)
	if len(got) > maxHistoryChars || !strings.HasSuffix(strings.TrimSpace(got), "n") || strings.Count(got, "### Comment") == 40 {
		t.Errorf("history not truncated from the oldest: %d chars, %d entries", len(got), strings.Count(got, "### Comment"))
	}
}

func TestCleanNote(t *testing.T) {
	body := "You're right.\n\n<sub>agent-nexus · job 7</sub>\n" + replyMarker(comment(5, "", "", 0)) + "\n"
	if got := cleanNote(body); got != "You're right." {
		t.Errorf("cleanNote = %q", got)
	}
}

func TestReplyBody(t *testing.T) {
	reply := inline(2, "alice", "a", "why?", 2)
	got := replyBody(ask{inline: true, covered: []forge.Note{reply, inline(3, "bob", "a", "+1", 3)}}, "Because.", resolvesMarker("review:7", []int{2}), 9)
	if strings.Contains(got, "> ") || !strings.HasPrefix(got, "Because.") ||
		!strings.Contains(got, "<sub>agent-nexus · job 9</sub>") ||
		!strings.Contains(got, replyMarker(reply)) || !strings.Contains(got, "reply-to=inline:3") || !strings.Contains(got, "<!-- agent-nexus:resolves=review:7#2 -->") {
		t.Errorf("inline reply body:\n%s", got)
	}
	mention := comment(5, "carol", "@agent-nexus is this safe?\nsecond line", 5)
	got = replyBody(ask{thread: []forge.Note{mention}, covered: []forge.Note{mention}}, "Yes.", "", 9)
	if !strings.HasPrefix(got, "> **@carol**:\n> @agent-nexus is this safe?\n> second line\n\nYes.") || !strings.Contains(got, replyMarker(mention)) {
		t.Errorf("mention reply body:\n%s", got)
	}
}
