package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/websocket/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/middleware"
)

const (
	chatMaxMessageLen      = 500
	chatMaxNameLen         = 32
	chatRateBurst          = 3.0 // messages
	chatRateRefill         = 3.0 // messages per second
	chatHistoryLimit       = 150
	chatDefaultMuteMinutes = 5
	chatMaxMuteMinutes     = 60
)

type chatMessage struct {
	Type   string `json:"type"` // "message" | "system" | "delete"
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Text   string `json:"text,omitempty"`
	At     int64  `json:"at"`
	IsHost bool   `json:"is_host,omitempty"`
}

// client wraps a websocket connection with a write mutex — gorilla/websocket
// (which gofiber/websocket wraps) does not allow concurrent writes to the same conn.
type chatClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (cl *chatClient) writeJSON(v any) {
	payload, err := json.Marshal(v)
	if err != nil {
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	_ = cl.conn.WriteMessage(websocket.TextMessage, payload)
}

// chatRoom is one event's chat: connected clients, a bounded scrollback so a
// newly-joined client isn't looking at an empty room mid-event, and a
// name -> mute-expiry map for the host's temporary mutes.
type chatRoom struct {
	clients    map[*chatClient]bool
	history    []chatMessage
	mutedUntil map[string]time.Time
}

// ChatHub keeps an in-memory, per-event room of connected clients and fans out
// broadcasts to all of them. Single-process only — messages aren't persisted
// and won't survive a server restart or be visible across multiple instances.
type ChatHub struct {
	mu    sync.Mutex
	rooms map[string]*chatRoom
}

func NewChatHub() *ChatHub {
	return &ChatHub{rooms: make(map[string]*chatRoom)}
}

func (h *ChatHub) room(eventID string) *chatRoom {
	r := h.rooms[eventID]
	if r == nil {
		r = &chatRoom{
			clients:    make(map[*chatClient]bool),
			mutedUntil: make(map[string]time.Time),
		}
		h.rooms[eventID] = r
	}
	return r
}

// join adds the client to the room and returns a snapshot of recent
// scrollback to replay to just this client.
func (h *ChatHub) join(eventID string, cl *chatClient) []chatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.room(eventID)
	r.clients[cl] = true
	history := make([]chatMessage, len(r.history))
	copy(history, r.history)
	return history
}

func (h *ChatHub) leave(eventID string, cl *chatClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[eventID]
	if r == nil {
		return
	}
	delete(r.clients, cl)
	if len(r.clients) == 0 {
		delete(h.rooms, eventID)
	}
}

func (h *ChatHub) broadcast(eventID string, msg chatMessage) {
	h.mu.Lock()
	r := h.room(eventID)
	if msg.Type == "message" || msg.Type == "system" {
		r.history = append(r.history, msg)
		if len(r.history) > chatHistoryLimit {
			r.history = r.history[len(r.history)-chatHistoryLimit:]
		}
	}
	recipients := make([]*chatClient, 0, len(r.clients))
	for cl := range r.clients {
		recipients = append(recipients, cl)
	}
	h.mu.Unlock()

	for _, cl := range recipients {
		cl.writeJSON(msg)
	}
}

// deleteMessage drops a message from scrollback (so late joiners never see
// it) and broadcasts a "delete" event so anyone already looking at it right
// now removes it from their view live.
func (h *ChatHub) deleteMessage(eventID, msgID string) {
	h.mu.Lock()
	r := h.rooms[eventID]
	var recipients []*chatClient
	if r != nil {
		for i, m := range r.history {
			if m.ID == msgID {
				r.history = append(r.history[:i], r.history[i+1:]...)
				break
			}
		}
		recipients = make([]*chatClient, 0, len(r.clients))
		for cl := range r.clients {
			recipients = append(recipients, cl)
		}
	}
	h.mu.Unlock()

	for _, cl := range recipients {
		cl.writeJSON(chatMessage{Type: "delete", ID: msgID, At: time.Now().UnixMilli()})
	}
}

// mute silences a display name (case/space-insensitive) for the given
// duration. Keyed by name rather than connection so it survives that
// viewer refreshing or reconnecting.
func (h *ChatHub) mute(eventID, name string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.room(eventID)
	r.mutedUntil[normalizeName(name)] = time.Now().Add(d)
}

// mutedFor reports how much longer name is muted, if at all, lazily
// clearing the entry once it's expired.
func (h *ChatHub) mutedFor(eventID, name string) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[eventID]
	if r == nil {
		return 0, false
	}
	key := normalizeName(name)
	until, ok := r.mutedUntil[key]
	if !ok {
		return 0, false
	}
	remaining := time.Until(until)
	if remaining <= 0 {
		delete(r.mutedUntil, key)
		return 0, false
	}
	return remaining, true
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

type ChatHandler struct {
	Hub *ChatHub
	DB  *pgxpool.Pool
	Cfg *config.Config
}

// authenticateHost checks a JWT (passed as ?token=) belongs to the actual
// host of this event — anyone else, including other hosts, connects as a
// regular viewer.
func (h *ChatHandler) authenticateHost(eventID, token string) bool {
	if token == "" {
		return false
	}
	claims := &middleware.Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(h.Cfg.JWTSecret), nil
	})
	if err != nil || !parsed.Valid || claims.Role != "host" {
		return false
	}

	var hostID string
	if err := h.DB.QueryRow(context.Background(),
		`SELECT host_id FROM events WHERE id = $1`, eventID,
	).Scan(&hostID); err != nil {
		return false
	}
	return hostID == claims.UserID
}

// HandleWS upgrades the connection and relays chat messages to everyone else
// watching the same event. Viewer auth is intentionally lightweight: the
// caller supplies a display name via ?name=, matching this app's current
// viewer flow. A connection additionally carrying a valid host ?token= for
// this exact event gets moderation privileges (delete/mute) and has its
// messages flagged is_host for the client to badge.
func (h *ChatHandler) HandleWS(c *websocket.Conn) {
	eventID := c.Params("id")
	isHost := h.authenticateHost(eventID, c.Query("token"))

	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		if isHost {
			name = "Host"
		} else {
			name = "Guest"
		}
	}
	name = truncateRunes(name, chatMaxNameLen)

	cl := &chatClient{conn: c}
	for _, m := range h.Hub.join(eventID, cl) {
		cl.writeJSON(m)
	}
	defer func() {
		h.Hub.leave(eventID, cl)
		h.Hub.broadcast(eventID, chatMessage{Type: "system", Text: name + " left the chat", At: time.Now().UnixMilli()})
	}()

	h.Hub.broadcast(eventID, chatMessage{Type: "system", Text: name + " joined the chat", At: time.Now().UnixMilli()})

	tokens := chatRateBurst
	lastRefill := time.Now()

	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			return
		}

		var incoming struct {
			Text    string `json:"text"`
			Action  string `json:"action"` // host-only: "delete" | "mute"
			Name    string `json:"name"`   // target display name, for "mute"
			Minutes int    `json:"minutes"`
			ID      string `json:"id"` // target message id, for "delete"
		}
		if err := json.Unmarshal(raw, &incoming); err != nil {
			continue
		}

		if isHost && incoming.Action != "" {
			h.handleModeration(eventID, incoming.Action, incoming.Name, incoming.ID, incoming.Minutes)
			continue
		}

		text := strings.TrimSpace(incoming.Text)
		if text == "" {
			continue
		}

		if !isHost {
			if remaining, muted := h.Hub.mutedFor(eventID, name); muted {
				cl.writeJSON(chatMessage{
					Type: "system",
					Text: fmt.Sprintf("You're muted for %d more minute(s).", int(remaining.Minutes())+1),
					At:   time.Now().UnixMilli(),
				})
				continue
			}
		}

		text = truncateRunes(text, chatMaxMessageLen)
		text = censor(text)

		// Token-bucket rate limit: a few messages per second per connection.
		now := time.Now()
		tokens += now.Sub(lastRefill).Seconds() * chatRateRefill
		if tokens > chatRateBurst {
			tokens = chatRateBurst
		}
		lastRefill = now
		if tokens < 1 {
			continue
		}
		tokens--

		h.Hub.broadcast(eventID, chatMessage{
			Type:   "message",
			ID:     uuid.NewString(),
			Name:   name,
			Text:   text,
			At:     now.UnixMilli(),
			IsHost: isHost,
		})
	}
}

func (h *ChatHandler) handleModeration(eventID, action, targetName, msgID string, minutes int) {
	switch action {
	case "delete":
		if msgID != "" {
			h.Hub.deleteMessage(eventID, msgID)
		}
	case "mute":
		target := strings.TrimSpace(targetName)
		if target == "" {
			return
		}
		if minutes <= 0 {
			minutes = chatDefaultMuteMinutes
		}
		if minutes > chatMaxMuteMinutes {
			minutes = chatMaxMuteMinutes
		}
		h.Hub.mute(eventID, target, time.Duration(minutes)*time.Minute)
		h.Hub.broadcast(eventID, chatMessage{
			Type: "system",
			Text: fmt.Sprintf("%s was muted for %d minute(s) by the host.", target, minutes),
			At:   time.Now().UnixMilli(),
		})
	}
}

// truncateRunes caps s at n runes instead of n bytes, so multi-byte
// characters (emoji included) never get sliced in half into invalid UTF-8.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

var profanityRegex = buildProfanityRegex([]string{
	"fuck", "shit", "bitch", "asshole", "bastard", "cunt", "dick", "piss",
	"crap", "slut", "whore", "nigger", "nigga", "faggot", "fag", "retard",
	"cock", "pussy", "damn",
})

func buildProfanityRegex(words []string) *regexp.Regexp {
	escaped := make([]string, len(words))
	for i, w := range words {
		escaped[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(escaped, "|") + `)\b`)
}

// censor replaces flagged words with their first letter followed by
// asterisks (e.g. "shit" -> "s***") instead of rejecting the message
// outright — word-list filters produce false positives, and silently
// blocking a message tends to just frustrate people over nothing.
func censor(text string) string {
	return profanityRegex.ReplaceAllStringFunc(text, func(match string) string {
		runes := []rune(match)
		if len(runes) <= 1 {
			return match
		}
		return string(runes[0]) + strings.Repeat("*", len(runes)-1)
	})
}
