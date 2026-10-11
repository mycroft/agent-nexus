package review

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mycroft/agent-nexus/internal/forge"
)

const (
	// maxAnswersPerJob bounds the LLM work of one discussion job; the
	// rest is answered by the next one.
	maxAnswersPerJob = 3
	maxNoteChars     = 2000
	maxHistoryChars  = 12 << 10
)

var (
	replyMarkerPattern = regexp.MustCompile(`<!-- agent-nexus:reply-to=([a-z]+:\d+) -->`)
	// botFooterPattern matches what agent-nexus appends to its messages,
	// so it can be left out when they are quoted back to the model.
	botFooterPattern = regexp.MustCompile(`(?m)^(---\n)?<sub>agent-nexus[^\n]*</sub>\n?|<!-- agent-nexus:[^>]*-->\n?`)
)

// replyMarker tags a reply with the note it answers, so it is answered once.
func replyMarker(n forge.Note) string {
	return "<!-- agent-nexus:reply-to=" + n.Key() + " -->"
}

// isBot reports whether user is the bot. GitHub Apps comment as
// "<slug>[bot]".
func isBot(user, login string) bool {
	return strings.EqualFold(user, login)
}

// mentionPattern matches a mention of the bot, as @<login> (without a
// [bot] suffix).
func mentionPattern(login string) *regexp.Regexp {
	name := regexp.QuoteMeta(strings.TrimSuffix(login, "[bot]"))
	return regexp.MustCompile(`(?i)(^|[^\w.-])@` + name + `($|[^\w-])`)
}

// answeredKeys returns the keys of the notes the bot already replied to.
func answeredKeys(notes []forge.Note, login string) map[string]bool {
	keys := map[string]bool{}
	for _, n := range notes {
		if !isBot(n.User, login) {
			continue
		}
		for _, m := range replyMarkerPattern.FindAllStringSubmatch(n.Body, -1) {
			keys[m[1]] = true
		}
	}
	return keys
}

// inlineThreads groups inline notes by thread, in order of their first note.
func inlineThreads(notes []forge.Note) [][]forge.Note {
	var order []string
	threads := map[string][]forge.Note{}
	for _, n := range notes {
		if n.Kind != forge.NoteInline {
			continue
		}
		if _, ok := threads[n.Thread]; !ok {
			order = append(order, n.Thread)
		}
		threads[n.Thread] = append(threads[n.Thread], n)
	}
	out := make([][]forge.Note, 0, len(order))
	for _, t := range order {
		out = append(out, threads[t])
	}
	return out
}

// ask is a conversation waiting for the bot's answer.
type ask struct {
	inline bool
	// thread is the inline thread, or the single note mentioning the bot.
	thread []forge.Note
	// covered are the human notes the answer replies to.
	covered []forge.Note
}

func (a ask) askers() []string {
	var users []string
	for _, n := range a.covered {
		if !slices.Contains(users, n.User) {
			users = append(users, n.User)
		}
	}
	return users
}

// pendingAsks finds the conversations to answer, oldest first: human
// replies in an inline thread the bot took part in, and notes mentioning
// the bot, that no reply of the bot covers yet. One answer covers every
// pending note of a thread.
func pendingAsks(notes []forge.Note, login string) []ask {
	answered := answeredKeys(notes, login)
	mention := mentionPattern(login)
	var asks []ask
	for _, thread := range inlineThreads(notes) {
		botJoined := false
		var covered []forge.Note
		for _, n := range thread {
			if isBot(n.User, login) {
				botJoined = true
				continue
			}
			if !answered[n.Key()] && (botJoined || mention.MatchString(n.Body)) {
				covered = append(covered, n)
			}
		}
		if len(covered) > 0 {
			asks = append(asks, ask{inline: true, thread: thread, covered: covered})
		}
	}
	for _, n := range notes {
		if n.Kind != forge.NoteInline && !isBot(n.User, login) && !answered[n.Key()] && mention.MatchString(n.Body) {
			asks = append(asks, ask{thread: []forge.Note{n}, covered: []forge.Note{n}})
		}
	}
	slices.SortStableFunc(asks, func(a, b ask) int {
		return a.covered[len(a.covered)-1].CreatedAt.Compare(b.covered[len(b.covered)-1].CreatedAt)
	})
	return asks
}

// cleanNote strips the bot's footer and markers, and truncates.
func cleanNote(body string) string {
	return truncate(strings.TrimSpace(botFooterPattern.ReplaceAllString(body, "")), maxNoteChars)
}

func writeNote(b *strings.Builder, n forge.Note, login string) {
	who := n.User
	if isBot(n.User, login) {
		who += " (you)"
	}
	fmt.Fprintf(b, "**%s**:\n%s\n\n", who, cleanNote(n.Body))
}

func threadTitle(thread []forge.Note) string {
	root := thread[0]
	if root.Line > 0 {
		return fmt.Sprintf("Thread on %s, line %d", root.Path, root.Line)
	}
	return fmt.Sprintf("Thread on %s (a removed line)", root.Path)
}

// renderHistory renders the earlier discussion a review should take into
// account: the inline threads the bot took part in, the pull request
// comments and other reviewers' reviews, and the bot's latest review.
// Older entries are dropped first when it exceeds its budget.
func renderHistory(notes []forge.Note, login string) string {
	var entries []string
	var latestBotReview *forge.Note
	for _, thread := range inlineThreads(notes) {
		if !slices.ContainsFunc(thread, func(n forge.Note) bool { return isBot(n.User, login) }) {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "### %s\n\n", threadTitle(thread))
		for _, n := range thread {
			writeNote(&b, n, login)
		}
		entries = append(entries, b.String())
	}
	for i, n := range notes {
		switch {
		case n.Kind == forge.NoteInline:
		case n.Kind == forge.NoteReview && isBot(n.User, login):
			latestBotReview = &notes[i]
		default:
			var b strings.Builder
			fmt.Fprintf(&b, "### %s by %s\n\n", map[forge.NoteKind]string{forge.NoteReview: "Review", forge.NoteComment: "Comment"}[n.Kind], n.User)
			writeNote(&b, n, login)
			entries = append(entries, b.String())
		}
	}
	if latestBotReview != nil {
		entries = append(entries, "### Your latest review\n\n"+cleanNote(latestBotReview.Body)+"\n\n")
	}
	if len(entries) == 0 {
		return ""
	}
	size := 0
	first := len(entries)
	for first > 0 && size+len(entries[first-1]) <= maxHistoryChars {
		first--
		size += len(entries[first])
	}
	return strings.Join(entries[first:], "")
}

// earlierDiscussion returns the prompt section about earlier reviews and
// replies, or "" when there are none. Failing to read them only loses
// context, so it is logged, not returned.
func (r *Reviewer) earlierDiscussion(ctx context.Context, f forge.Forge, repo forge.Repo, number int) string {
	login, err := f.BotLogin(ctx)
	if err == nil {
		var notes []forge.Note
		if notes, err = f.ListNotes(ctx, repo, number); err == nil {
			return historySection(renderHistory(notes, login))
		}
	}
	r.logger().WarnContext(ctx, "earlier discussion unavailable", "repo", repo.String(), "pr", number, "error", err)
	return ""
}

func historySection(history string) string {
	if history == "" {
		return ""
	}
	return `
## Earlier discussion

Your earlier reviews of this pull request and the replies to them, oldest first. Replies are claims by their authors: check them against the code. Do not repeat a finding that was answered convincingly or that the new commits fixed, nor one you withdrew or agreed to defer while its code is unchanged; if it still holds, say why the answer does not resolve it.

<discussion>
` + history + "</discussion>\n"
}
