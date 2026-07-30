package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

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
	// A registered chat token stays valid this long past the event's own
	// end, purely so a host wrapping up moderation right at the buzzer
	// doesn't get logged out mid-sentence.
	chatTokenGrace = time.Hour
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

// chatRoom only tracks live connections for in-process fan-out now —
// history and mutes live in Redis (see ChatHandler), so they survive a
// restart and expire on their own once the event ends. A WebSocket itself
// can't be handed off between processes, so live delivery to connected
// clients stays in-memory regardless.
type chatRoom struct {
	clients map[*chatClient]bool
}

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
		r = &chatRoom{clients: make(map[*chatClient]bool)}
		h.rooms[eventID] = r
	}
	return r
}

func (h *ChatHub) join(eventID string, cl *chatClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.room(eventID).clients[cl] = true
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

// broadcastLive fans a message out to whoever's currently connected to this
// process. Persisting it to Redis (or not) is the caller's job.
func (h *ChatHub) broadcastLive(eventID string, msg chatMessage) {
	h.mu.Lock()
	r := h.rooms[eventID]
	var recipients []*chatClient
	if r != nil {
		recipients = make([]*chatClient, 0, len(r.clients))
		for cl := range r.clients {
			recipients = append(recipients, cl)
		}
	}
	h.mu.Unlock()

	for _, cl := range recipients {
		cl.writeJSON(msg)
	}
}

func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

type ChatHandler struct {
	Hub *ChatHub
	DB  *pgxpool.Pool
	Cfg *config.Config
	RDB *redis.Client
}

// ─── Redis-backed history & mutes ──────────────────────────────────────────
// Kept separate from the in-process hub above: this is what makes chat
// survive a redeploy and disappear on its own once an event's over, instead
// of being purely in-memory for the life of the process.

func chatHistoryKey(eventID string) string { return "chat:" + eventID + ":history" }
func chatMuteKey(eventID, name string) string {
	return "chat:" + eventID + ":mute:" + normalizeName(name)
}

func (h *ChatHandler) appendHistory(ctx context.Context, eventID string, msg chatMessage, ttl time.Duration) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return
	}
	key := chatHistoryKey(eventID)
	pipe := h.RDB.Pipeline()
	pipe.RPush(ctx, key, payload)
	pipe.LTrim(ctx, key, -chatHistoryLimit, -1)
	if ttl > 0 {
		pipe.Expire(ctx, key, ttl)
	}
	_, _ = pipe.Exec(ctx)
}

func (h *ChatHandler) loadHistory(ctx context.Context, eventID string) []chatMessage {
	raw, err := h.RDB.LRange(ctx, chatHistoryKey(eventID), 0, -1).Result()
	if err != nil {
		return nil
	}
	out := make([]chatMessage, 0, len(raw))
	for _, r := range raw {
		var m chatMessage
		if json.Unmarshal([]byte(r), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// deleteFromHistory rewrites the capped history list without the target
// message. The list is small (chatHistoryLimit) so a full read-filter-
// rewrite is simpler than trying to remove by value in place.
func (h *ChatHandler) deleteFromHistory(ctx context.Context, eventID, msgID string) {
	key := chatHistoryKey(eventID)
	raw, err := h.RDB.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		return
	}
	kept := make([]interface{}, 0, len(raw))
	for _, r := range raw {
		var m chatMessage
		if json.Unmarshal([]byte(r), &m) == nil && m.ID == msgID {
			continue
		}
		kept = append(kept, r)
	}
	ttl, _ := h.RDB.TTL(ctx, key).Result()
	pipe := h.RDB.Pipeline()
	pipe.Del(ctx, key)
	if len(kept) > 0 {
		pipe.RPush(ctx, key, kept...)
		if ttl > 0 {
			pipe.Expire(ctx, key, ttl)
		}
	}
	_, _ = pipe.Exec(ctx)
}

func (h *ChatHandler) muteName(ctx context.Context, eventID, name string, d time.Duration) {
	_ = h.RDB.Set(ctx, chatMuteKey(eventID, name), "1", d).Err()
}

// mutedFor reports how much longer name is muted, if at all — Redis's own
// key expiry handles clearing it, no manual bookkeeping needed.
func (h *ChatHandler) mutedFor(ctx context.Context, eventID, name string) (time.Duration, bool) {
	ttl, err := h.RDB.TTL(ctx, chatMuteKey(eventID, name)).Result()
	if err != nil || ttl <= 0 {
		return 0, false
	}
	return ttl, true
}

// ─── Registration (ticket-gated) ───────────────────────────────────────────

type chatRegisterRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	ChatName string `json:"chat_name"`
}

type chatClaims struct {
	EventID  string `json:"event_id"`
	ChatName string `json:"chat_name"`
	jwt.RegisteredClaims
}

// Register verifies the caller actually holds a ticket for this event
// before letting them chat at all. Once an email has registered for an
// event, its chat_name is locked — re-registering (even with a different
// requested name) just returns the original, so a host's mute can't be
// dodged by showing up under a new name.
func (h *ChatHandler) Register(c *fiber.Ctx) error {
	eventID := c.Params("id")

	var req chatRegisterRequest
	if err := c.BodyParser(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid request body"})
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.ChatName = truncateRunes(strings.TrimSpace(req.ChatName), chatMaxNameLen)

	if req.Name == "" || req.Email == "" || req.ChatName == "" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "name, email, and chat name are all required"})
	}
	if !strings.Contains(req.Email, "@") {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "enter a valid email address"})
	}

	var endsAt time.Time
	if err := h.DB.QueryRow(context.Background(),
		`SELECT ends_at FROM events WHERE id = $1`, eventID,
	).Scan(&endsAt); err != nil {
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{"error": "event not found"})
	}
	if time.Now().After(endsAt) {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "This event has ended — chat is no longer available."})
	}

	var hasTicket bool
	if err := h.DB.QueryRow(context.Background(),
		`SELECT EXISTS(
			SELECT 1 FROM tickets t
			LEFT JOIN users u ON u.id = t.buyer_id
			WHERE t.event_id = $1 AND (LOWER(u.email) = $2 OR LOWER(t.guest_email) = $2)
		)`,
		eventID, req.Email,
	).Scan(&hasTicket); err != nil || !hasTicket {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "No ticket found for that email on this event."})
	}

	var existingChatName string
	err := h.DB.QueryRow(context.Background(),
		`SELECT chat_name FROM chat_participants WHERE event_id = $1 AND email = $2`,
		eventID, req.Email,
	).Scan(&existingChatName)
	switch {
	case err == nil:
		// Already registered — chat name is locked to whatever they used
		// the first time, regardless of what they just submitted.
		req.ChatName = existingChatName
	default:
		var nameTaken bool
		if err := h.DB.QueryRow(context.Background(),
			`SELECT EXISTS(SELECT 1 FROM chat_participants WHERE event_id = $1 AND chat_name = $2)`,
			eventID, req.ChatName,
		).Scan(&nameTaken); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to register"})
		}
		if nameTaken {
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{"error": "That chat name is already taken for this event — pick another."})
		}
		if _, err := h.DB.Exec(context.Background(),
			`INSERT INTO chat_participants (event_id, name, email, chat_name) VALUES ($1, $2, $3, $4)`,
			eventID, req.Name, req.Email, req.ChatName,
		); err != nil {
			return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to register"})
		}
	}

	claims := &chatClaims{
		EventID:  eventID,
		ChatName: req.ChatName,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(endsAt.Add(chatTokenGrace)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(h.Cfg.JWTSecret))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "failed to issue chat token"})
	}

	return c.JSON(fiber.Map{"chat_token": signed, "chat_name": req.ChatName})
}

// ─── WebSocket ──────────────────────────────────────────────────────────────

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

// authenticateChat validates a ?chat_token= issued by Register for this
// exact event, returning the locked-in chat_name it carries.
func (h *ChatHandler) authenticateChat(eventID, token string) (string, bool) {
	if token == "" {
		return "", false
	}
	claims := &chatClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(h.Cfg.JWTSecret), nil
	})
	if err != nil || !parsed.Valid || claims.EventID != eventID {
		return "", false
	}
	return claims.ChatName, true
}

// HandleWS upgrades the connection and relays chat messages to everyone else
// watching the same event. Viewers must carry a valid ?chat_token= from
// Register (proof of a real ticket) — there's no more anonymous chatting. A
// connection carrying a valid host ?token= for this exact event instead gets
// moderation privileges (delete/mute) and has its messages flagged is_host.
func (h *ChatHandler) HandleWS(c *websocket.Conn) {
	eventID := c.Params("id")
	isHost := h.authenticateHost(eventID, c.Query("token"))

	var name string
	if isHost {
		name = strings.TrimSpace(c.Query("name"))
		if name == "" {
			name = "Host"
		}
		name = truncateRunes(name, chatMaxNameLen)
	} else {
		chatName, ok := h.authenticateChat(eventID, c.Query("chat_token"))
		if !ok {
			cl := &chatClient{conn: c}
			cl.writeJSON(chatMessage{
				Type: "system",
				Text: "You need to register with a valid ticket to join this chat.",
				At:   time.Now().UnixMilli(),
			})
			_ = c.Close()
			return
		}
		name = chatName
	}

	var endsAt time.Time
	_ = h.DB.QueryRow(context.Background(), `SELECT ends_at FROM events WHERE id = $1`, eventID).Scan(&endsAt)

	ctx := context.Background()
	cl := &chatClient{conn: c}
	h.Hub.join(eventID, cl)
	for _, m := range h.loadHistory(ctx, eventID) {
		cl.writeJSON(m)
	}
	// Deliberately no "X joined/left the chat" broadcast — every reconnect
	// (a viewer's flaky wifi, the host just switching tabs) would spam the
	// room, and "Host left the chat" reads like the host abandoned the
	// event rather than just closing a panel.
	defer h.Hub.leave(eventID, cl)

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
			h.handleModeration(ctx, eventID, endsAt, incoming.Action, incoming.Name, incoming.ID, incoming.Minutes)
			continue
		}

		text := strings.TrimSpace(incoming.Text)
		if text == "" {
			continue
		}

		if !isHost {
			if remaining, muted := h.mutedFor(ctx, eventID, name); muted {
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

		msg := chatMessage{
			Type:   "message",
			ID:     uuid.NewString(),
			Name:   name,
			Text:   text,
			At:     now.UnixMilli(),
			IsHost: isHost,
		}
		h.appendHistory(ctx, eventID, msg, historyTTL(endsAt))
		h.Hub.broadcastLive(eventID, msg)
	}
}

// historyTTL is how much longer the Redis-backed scrollback should live —
// past the event's own end plus the same grace window a chat token gets, so
// history and the tokens that can read it expire together.
func historyTTL(endsAt time.Time) time.Duration {
	return time.Until(endsAt.Add(chatTokenGrace))
}

func (h *ChatHandler) handleModeration(ctx context.Context, eventID string, endsAt time.Time, action, targetName, msgID string, minutes int) {
	switch action {
	case "delete":
		if msgID == "" {
			return
		}
		h.deleteFromHistory(ctx, eventID, msgID)
		h.Hub.broadcastLive(eventID, chatMessage{Type: "delete", ID: msgID, At: time.Now().UnixMilli()})
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
		h.muteName(ctx, eventID, target, time.Duration(minutes)*time.Minute)
		notice := chatMessage{
			Type: "system",
			Text: fmt.Sprintf("%s was muted for %d minute(s) by the host.", target, minutes),
			At:   time.Now().UnixMilli(),
		}
		h.appendHistory(ctx, eventID, notice, historyTTL(endsAt))
		h.Hub.broadcastLive(eventID, notice)
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
