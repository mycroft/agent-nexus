// Package zuliptest provides an in-memory Zulip server for tests: the
// API calls of zulip.Client over the conversations it holds.
package zuliptest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mycroft/agent-nexus/internal/httpclient"
	"github.com/mycroft/agent-nexus/internal/zulip"
)

// Bot is the fake server's bot user.
var Bot = zulip.Me{ID: 99, Email: "agent-nexus-bot@zulip.example.com", FullName: "agent-nexus"}

const botKey = "bot-api-key"

// Fake is a Zulip server.
type Fake struct {
	srv *httptest.Server

	mu       sync.Mutex
	messages []zulip.Message
	sent     []zulip.Message
	nextID   int64
	// FailSend makes posting messages fail.
	FailSend bool
}

// New starts a fake server, stopped when the test ends.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{nextID: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/me", f.auth(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"result": "success", "user_id": Bot.ID, "email": Bot.Email, "full_name": Bot.FullName})
	}))
	mux.HandleFunc("GET /api/v1/messages", f.auth(f.history))
	mux.HandleFunc("POST /api/v1/messages", f.auth(f.send))
	mux.HandleFunc("POST /api/v1/messages/{id}/reactions", f.auth(f.react))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// URL is the server's root URL.
func (f *Fake) URL() string { return f.srv.URL }

// Client returns a client of the server, authenticated as Bot.
func (f *Fake) Client() *zulip.Client {
	return zulip.New(f.srv.URL, Bot.Email, botKey, httpclient.WithRetry(0, 0, 0))
}

// Add stores a message and returns it with its ID.
func (f *Fake) Add(m zulip.Message) zulip.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	m.ID = f.nextID
	f.messages = append(f.messages, m)
	return m
}

// Sent returns the messages the bot posted.
func (f *Fake) Sent() []zulip.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// Message returns the stored message with this ID.
func (f *Fake) Message(id int64) zulip.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.ID == id {
			return m
		}
	}
	return zulip.Message{}
}

// StreamMessage builds a channel message.
func StreamMessage(streamID int64, stream, topic string, sender zulip.User, content string) zulip.Message {
	recipient, _ := json.Marshal(stream)
	return zulip.Message{
		Type: "stream", StreamID: streamID, Subject: topic, DisplayRecipient: recipient,
		SenderID: sender.ID, SenderFullName: sender.FullName, SenderEmail: email(sender), Content: content,
	}
}

// DirectMessage builds a direct message from sender to the bot.
func DirectMessage(sender zulip.User, content string) zulip.Message {
	recipient, _ := json.Marshal([]map[string]any{
		{"id": sender.ID, "email": email(sender), "full_name": sender.FullName},
		{"id": Bot.ID, "email": Bot.Email, "full_name": Bot.FullName},
	})
	return zulip.Message{
		Type: "private", DisplayRecipient: recipient,
		SenderID: sender.ID, SenderFullName: sender.FullName, SenderEmail: email(sender), Content: content,
	}
}

func email(u zulip.User) string {
	return strings.ToLower(strings.ReplaceAll(u.FullName, " ", ".")) + "@example.com"
}

func (f *Fake) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, key, ok := r.BasicAuth()
		if !ok || user != Bot.Email || key != botKey {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"result": "error", "msg": "Invalid API key"})
			return
		}
		next(w, r)
	}
}

// history serves GET /messages for a channel topic or a direct message
// conversation narrow.
func (f *Fake) history(w http.ResponseWriter, r *http.Request) {
	var narrow []struct {
		Operator string          `json:"operator"`
		Operand  json.RawMessage `json:"operand"`
	}
	if err := json.Unmarshal([]byte(r.URL.Query().Get("narrow")), &narrow); err != nil || r.URL.Query().Get("apply_markdown") != "false" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "bad request"})
		return
	}
	var streamID int64
	var topic string
	var users []int64
	for _, term := range narrow {
		switch term.Operator {
		case "stream":
			_ = json.Unmarshal(term.Operand, &streamID)
		case "topic":
			_ = json.Unmarshal(term.Operand, &topic)
		case "dm":
			_ = json.Unmarshal(term.Operand, &users)
		}
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("num_before"))
	f.mu.Lock()
	var out []zulip.Message
	for _, m := range f.messages {
		inStream := streamID != 0 && m.StreamID == streamID && strings.EqualFold(m.Subject, topic)
		inDM := len(users) > 0 && m.Type == "private"
		if inStream || inDM {
			out = append(out, m)
		}
	}
	f.mu.Unlock()
	out = out[max(0, len(out)-n):]
	writeJSON(w, http.StatusOK, map[string]any{"result": "success", "messages": out})
}

// send stores the bot's message in the conversation it is sent to.
func (f *Fake) send(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "bad form"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailSend {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"result": "error", "msg": "unavailable"})
		return
	}
	m := zulip.Message{SenderID: Bot.ID, SenderFullName: Bot.FullName, SenderEmail: Bot.Email, Content: r.PostForm.Get("content")}
	switch r.PostForm.Get("type") {
	case "stream":
		id, err := strconv.ParseInt(r.PostForm.Get("to"), 10, 64)
		if err != nil || r.PostForm.Get("topic") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "bad channel message"})
			return
		}
		m.Type, m.StreamID, m.Subject = "stream", id, r.PostForm.Get("topic")
	case "private":
		var users []int64
		if err := json.Unmarshal([]byte(r.PostForm.Get("to")), &users); err != nil || len(users) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "bad recipients"})
			return
		}
		m.Type = "private"
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "bad type"})
		return
	}
	f.nextID++
	m.ID = f.nextID
	f.messages = append(f.messages, m)
	f.sent = append(f.sent, m)
	writeJSON(w, http.StatusOK, map[string]any{"result": "success", "id": m.ID})
}

// react adds the bot's reaction to a message.
func (f *Fake) react(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	emoji := r.FormValue("emoji_name")
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, m := range f.messages {
		if m.ID != id {
			continue
		}
		if m.ReactedBy(Bot.ID, emoji) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "Reaction already exists.", "code": "REACTION_ALREADY_EXISTS"})
			return
		}
		f.messages[i].Reactions = append(f.messages[i].Reactions, zulip.Reaction{EmojiName: emoji, UserID: Bot.ID})
		writeJSON(w, http.StatusOK, map[string]any{"result": "success"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"result": "error", "msg": "Invalid message(s)"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
