package handlers

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/websocket/v2"
	"github.com/google/uuid"
)

const (
	chatMaxMessageLen = 500
	chatMaxNameLen    = 32
	chatRateBurst     = 3.0 // messages
	chatRateRefill    = 3.0 // messages per second
)

type chatMessage struct {
	Type string `json:"type"` // "message" | "system"
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Text string `json:"text"`
	At   int64  `json:"at"`
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

// ChatHub keeps an in-memory, per-event room of connected clients and fans out
// broadcasts to all of them. Single-process only — messages aren't persisted
// and won't survive a server restart or be visible across multiple instances.
type ChatHub struct {
	mu    sync.Mutex
	rooms map[string]map[*chatClient]bool
}

func NewChatHub() *ChatHub {
	return &ChatHub{rooms: make(map[string]map[*chatClient]bool)}
}

func (h *ChatHub) join(eventID string, cl *chatClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rooms[eventID] == nil {
		h.rooms[eventID] = make(map[*chatClient]bool)
	}
	h.rooms[eventID][cl] = true
}

func (h *ChatHub) leave(eventID string, cl *chatClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms[eventID], cl)
	if len(h.rooms[eventID]) == 0 {
		delete(h.rooms, eventID)
	}
}

func (h *ChatHub) broadcast(eventID string, msg chatMessage) {
	h.mu.Lock()
	recipients := make([]*chatClient, 0, len(h.rooms[eventID]))
	for cl := range h.rooms[eventID] {
		recipients = append(recipients, cl)
	}
	h.mu.Unlock()

	for _, cl := range recipients {
		cl.writeJSON(msg)
	}
}

type ChatHandler struct {
	Hub *ChatHub
}

// HandleWS upgrades the connection and relays chat messages to everyone else
// watching the same event. Auth is intentionally lightweight: the caller
// supplies a display name via ?name=, matching this app's current viewer flow.
func (h *ChatHandler) HandleWS(c *websocket.Conn) {
	eventID := c.Params("id")

	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		name = "Guest"
	}
	name = truncateRunes(name, chatMaxNameLen)

	cl := &chatClient{conn: c}
	h.Hub.join(eventID, cl)
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
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &incoming); err != nil {
			continue
		}

		text := strings.TrimSpace(incoming.Text)
		if text == "" {
			continue
		}
		text = truncateRunes(text, chatMaxMessageLen)

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
			Type: "message",
			ID:   uuid.NewString(),
			Name: name,
			Text: text,
			At:   now.UnixMilli(),
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
