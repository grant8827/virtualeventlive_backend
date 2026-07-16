package routes

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/websocket/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/handlers"
	"vertualeventlive/backend/middleware"
	"vertualeventlive/backend/services"
)

func Register(app *fiber.App, db *pgxpool.Pool, rdb *redis.Client, cfg *config.Config) {
	emailSvc := &services.EmailService{
		APIKey:    cfg.ResendAPIKey,
		FromEmail: cfg.FromEmail,
		SiteURL:   cfg.FrontendURL,
	}
	ivsSvc := services.NewIVSService(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, cfg.AWSRegion)

	// Health check
	health := &handlers.HealthHandler{DB: db, RDB: rdb}
	app.Get("/health", health.Check)

	// Stripe webhook — before body parser; needs raw body intact
	stripeH := &handlers.StripeHandler{DB: db, Cfg: cfg, Email: emailSvc, IVS: ivsSvc}
	app.Post("/api/v1/webhooks/stripe", stripeH.Webhook)

	v1 := app.Group("/api/v1")

	// Auth
	authH := &handlers.AuthHandler{DB: db, Cfg: cfg}
	v1.Post("/auth/register", authH.Register)
	v1.Post("/auth/login", authH.Login)
	v1.Post("/auth/logout", authH.Logout)
	v1.Get("/auth/me", middleware.Protected(cfg.JWTSecret), authH.Me)

	// Events
	eventH := &handlers.EventHandler{DB: db, Cfg: cfg}
	v1.Get("/events/public", eventH.ListPublic)
	v1.Get("/events/:id", eventH.GetByID)
	v1.Post("/events", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), eventH.Create)
	v1.Get("/events", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), eventH.ListByHost)
	v1.Post("/events/:id/checkout", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), eventH.Checkout)
	v1.Patch("/events/:id/ticket", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), eventH.TicketSetup)
	v1.Post("/events/:id/bypass-activate", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), eventH.BypassActivate)

	// Advertisements
	adH := &handlers.AdvertisementHandler{DB: db}
	v1.Get("/advertisements", adH.ListPublic)
	v1.Get("/advertisements/mine", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), adH.ListByHost)
	v1.Post("/advertisements", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), adH.Create)
	v1.Put("/advertisements/:id", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), adH.Update)
	v1.Delete("/advertisements/:id", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), adH.Delete)

	// Stream credentials — host only, returns IVS ingest URL + stream key
	credH := &handlers.StreamCredentialsHandler{DB: db, IVS: ivsSvc}
	v1.Get("/events/:id/stream-credentials", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), credH.Get)
	// Public — ticket holders poll this to know if the host is live right now
	v1.Get("/events/:id/stream-status", credH.Status)

	// Stripe Connect (host onboarding)
	v1.Post("/connect/onboard", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), stripeH.ConnectOnboard)
	v1.Get("/connect/status", middleware.Protected(cfg.JWTSecret), middleware.RequireRole("host"), stripeH.ConnectStatus)

	// Tickets
	ticketH := &handlers.TicketHandler{DB: db, Cfg: cfg, Email: emailSvc}
	v1.Get("/tickets/lookup", ticketH.Lookup)
	v1.Get("/tickets/enter", ticketH.Enter)
	v1.Post("/tickets/guest-purchase", ticketH.GuestPurchase)
	v1.Post("/tickets/purchase", middleware.Protected(cfg.JWTSecret), ticketH.Purchase)
	v1.Get("/tickets/mine", middleware.Protected(cfg.JWTSecret), ticketH.ListMine)

	// Viewer stream — Redis session locking
	guard := &services.SessionGuard{RDB: rdb}
	streamH := &handlers.StreamHandler{DB: db, Guard: guard}
	v1.Post("/stream/watch", middleware.Protected(cfg.JWTSecret), streamH.Watch)
	v1.Post("/stream/heartbeat", middleware.Protected(cfg.JWTSecret), streamH.Heartbeat)
	v1.Post("/stream/release", middleware.Protected(cfg.JWTSecret), streamH.Release)

	// Live chat — in-memory per-event room over WebSocket
	chatH := &handlers.ChatHandler{Hub: handlers.NewChatHub()}
	v1.Use("/events/:id/chat/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	v1.Get("/events/:id/chat/ws", websocket.New(chatH.HandleWS))
}
