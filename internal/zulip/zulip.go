// Package zulip talks to a Zulip server as a bot: it decodes the requests
// of the bot's outgoing webhook, reads conversations, posts answers and
// reacts to messages.
package zulip

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mycroft/agent-nexus/internal/httpclient"
)

// Outgoing is the body of an outgoing webhook request: Zulip sends one
// when the bot is mentioned in a channel, or sent a direct message.
type Outgoing struct {
	BotEmail    string `json:"bot_email"`
	BotFullName string `json:"bot_full_name"`
	// Token authenticates the request: the bot's outgoing webhook token.
	Token string `json:"token"`
	// Trigger is "mention" or "direct_message".
	Trigger string  `json:"trigger"`
	Message Message `json:"message"`
}

// Message is a Zulip message, with its content as raw Markdown.
type Message struct {
	ID             int64  `json:"id"`
	SenderID       int64  `json:"sender_id"`
	SenderEmail    string `json:"sender_email"`
	SenderFullName string `json:"sender_full_name"`
	Content        string `json:"content"`
	Timestamp      int64  `json:"timestamp"`
	// Type is "stream" for a channel message, "private" for a direct one.
	Type     string `json:"type"`
	StreamID int64  `json:"stream_id"`
	// Subject is the topic of a channel message.
	Subject string `json:"subject"`
	// DisplayRecipient is the channel name, or the users of a direct
	// message conversation.
	DisplayRecipient json.RawMessage `json:"display_recipient"`
	Reactions        []Reaction      `json:"reactions"`
}

// Reaction is an emoji reaction to a message.
type Reaction struct {
	EmojiName string `json:"emoji_name"`
	UserID    int64  `json:"user_id"`
}

// ReactedBy reports whether user reacted to m with emoji.
func (m Message) ReactedBy(user int64, emoji string) bool {
	return slices.ContainsFunc(m.Reactions, func(r Reaction) bool { return r.UserID == user && r.EmojiName == emoji })
}

// User is a participant of a conversation.
type User struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// Conversation is where a message was sent: a channel topic, or a direct
// message conversation.
type Conversation struct {
	StreamID int64  `json:"stream_id,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Topic    string `json:"topic,omitempty"`
	// Users are the participants of a direct message conversation, the
	// bot excepted, by ID.
	Users []User `json:"users,omitempty"`
}

// ConversationOf returns the conversation of m, as seen by the bot with
// the email botEmail.
func ConversationOf(m Message, botEmail string) (Conversation, error) {
	switch m.Type {
	case "stream":
		var name string
		if err := json.Unmarshal(m.DisplayRecipient, &name); err != nil || m.StreamID <= 0 {
			return Conversation{}, errors.New("invalid channel message recipient")
		}
		return Conversation{StreamID: m.StreamID, Stream: name, Topic: m.Subject}, nil
	case "private":
		var recipients []struct {
			ID       int64  `json:"id"`
			Email    string `json:"email"`
			FullName string `json:"full_name"`
		}
		if err := json.Unmarshal(m.DisplayRecipient, &recipients); err != nil {
			return Conversation{}, errors.New("invalid direct message recipients")
		}
		var c Conversation
		for _, r := range recipients {
			if !strings.EqualFold(r.Email, botEmail) && r.ID > 0 {
				c.Users = append(c.Users, User{ID: r.ID, FullName: r.FullName})
			}
		}
		if len(c.Users) == 0 {
			return Conversation{}, errors.New("direct message without another participant")
		}
		slices.SortFunc(c.Users, func(a, b User) int { return cmp.Compare(a.ID, b.ID) })
		return c, nil
	default:
		return Conversation{}, fmt.Errorf("unknown message type %q", m.Type)
	}
}

// Direct reports whether c is a direct message conversation.
func (c Conversation) Direct() bool { return c.StreamID == 0 }

// Key identifies the conversation. Zulip compares topics without case.
func (c Conversation) Key() string {
	if !c.Direct() {
		return fmt.Sprintf("stream:%d:%s", c.StreamID, strings.ToLower(c.Topic))
	}
	ids := make([]string, len(c.Users))
	for i, u := range c.Users {
		ids[i] = strconv.FormatInt(u.ID, 10)
	}
	return "dm:" + strings.Join(ids, ",")
}

func (c Conversation) String() string {
	if !c.Direct() {
		return "#" + c.Stream + " > " + c.Topic
	}
	names := make([]string, len(c.Users))
	for i, u := range c.Users {
		names[i] = u.FullName
	}
	return "direct messages with " + strings.Join(names, ", ")
}

// narrow returns the conversation as a GET /messages narrow.
func (c Conversation) narrow() ([]byte, error) {
	type term struct {
		Operator string `json:"operator"`
		Operand  any    `json:"operand"`
	}
	if !c.Direct() {
		return json.Marshal([]term{{"stream", c.StreamID}, {"topic", c.Topic}})
	}
	ids := make([]int64, len(c.Users))
	for i, u := range c.Users {
		ids[i] = u.ID
	}
	return json.Marshal([]term{{"dm", ids}})
}

// Client calls the Zulip API as a bot.
type Client struct {
	baseURL string
	email   string
	apiKey  string
	http    *httpclient.Client

	mu sync.Mutex
	me *Me
}

// New returns a client of the Zulip server at baseURL
// (https://zulip.example.com), authenticated as the bot with this email
// and API key.
func New(baseURL, email, apiKey string, opts ...httpclient.Option) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		email:   email,
		apiKey:  apiKey,
		http:    httpclient.New(opts...),
	}
}

// Me is the bot's own user.
type Me struct {
	ID       int64  `json:"user_id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

// Me returns the bot's user.
func (c *Client) Me(ctx context.Context) (Me, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.me != nil {
		return *c.me, nil
	}
	var me Me
	if err := c.do(ctx, http.MethodGet, "/users/me", nil, &me); err != nil {
		return Me{}, err
	}
	c.me = &me
	return me, nil
}

// History returns the latest n messages of a conversation, oldest first.
func (c *Client) History(ctx context.Context, conv Conversation, n int) ([]Message, error) {
	narrow, err := conv.narrow()
	if err != nil {
		return nil, fmt.Errorf("zulip: narrow: %w", err)
	}
	q := url.Values{
		"anchor":         {"newest"},
		"num_before":     {strconv.Itoa(n)},
		"num_after":      {"0"},
		"apply_markdown": {"false"},
		"narrow":         {string(narrow)},
	}
	var out struct {
		Messages []Message `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, "/messages?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Messages, nil
}

// Send posts content in a conversation and returns the new message's ID.
func (c *Client) Send(ctx context.Context, conv Conversation, content string) (int64, error) {
	form := url.Values{"content": {content}}
	if conv.Direct() {
		ids := make([]int64, len(conv.Users))
		for i, u := range conv.Users {
			ids[i] = u.ID
		}
		to, err := json.Marshal(ids)
		if err != nil {
			return 0, fmt.Errorf("zulip: recipients: %w", err)
		}
		form.Set("type", "private")
		form.Set("to", string(to))
	} else {
		form.Set("type", "stream")
		form.Set("to", strconv.FormatInt(conv.StreamID, 10))
		form.Set("topic", conv.Topic)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/messages", form, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// React adds the bot's emoji reaction to a message. Reacting twice is
// not an error.
func (c *Client) React(ctx context.Context, messageID int64, emoji string) error {
	path := fmt.Sprintf("/messages/%d/reactions", messageID)
	err := c.do(ctx, http.MethodPost, path, url.Values{"emoji_name": {emoji}}, nil)
	var statusErr *httpclient.StatusError
	if errors.As(err, &statusErr) && strings.Contains(statusErr.Body, "REACTION_ALREADY_EXISTS") {
		return nil
	}
	return err
}

// do sends a request to the API, with form as the body, and decodes the
// JSON response into out (if not nil).
func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, body)
	if err != nil {
		return fmt.Errorf("zulip: build %s %s: %w", method, path, err)
	}
	req.SetBasicAuth(c.email, c.apiKey)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zulip: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := httpclient.Read(resp, 16<<20)
	if err != nil {
		return fmt.Errorf("zulip: %w", err)
	}
	if err := httpclient.EnsureOK(resp, raw); err != nil {
		return fmt.Errorf("zulip: %s %s: %w", method, path, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("zulip: decode %s %s: %w", method, path, err)
	}
	return nil
}
