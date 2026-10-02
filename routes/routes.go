package routes

import (
	"log"

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
	// Sign-in is a server-side session in an HttpOnly cookie (AUTH-03).
	// Secure everywhere except local development over plain http.
	sessionCfg := middleware.SessionConfig{Secure: !cfg.IsDevelopment()}
	requireSession := middleware.RequireSession(db, sessionCfg)

	// Defensive headers on every response, and the CSRF origin check for
	// cookie-authenticated requests that change state.
	app.Use(middleware.SecurityHeaders(!cfg.IsDevelopment()))
	trustedOrigins := middleware.NormalizeOrigins(cfg.CorsAllowedOrigins, cfg.FrontendURL)

	emailSvc := &services.EmailService{
		APIKey:       cfg.ResendAPIKey,
		FromEmail:    cfg.FromEmail,
		SiteURL:      cfg.FrontendURL,
		SMTPHost:     cfg.SMTPHost,
		SMTPPort:     cfg.SMTPPort,
		SMTPUsername: cfg.SMTPUsername,
		SMTPPassword: cfg.SMTPPassword,
	}
	ivsSvc := services.NewIVSService(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, cfg.AWSRegion)
	ivsSvc.AuthorizedChannels = cfg.IVSPlaybackAuthRequired

	// IVS private playback: playback URLs are only issued after a server-side
	// ticket check, signed with a short-lived token when a key is configured.
	playback := &handlers.PlaybackAccess{Required: cfg.IVSPlaybackAuthRequired}
	if cfg.IVSPlaybackPrivateKey != "" {
		signer, err := services.NewPlaybackSigner(cfg.IVSPlaybackPrivateKey, cfg.IVSPlaybackAllowOrigin, services.DefaultPlaybackTokenTTL)
		if err != nil {
			log.Fatalf("IVS playback signing: %v", err)
		}
		playback.Signer = signer
	}
	s3Storage := services.NewS3Storage(
		cfg.AWSAccessKeyID,
		cfg.AWSSecretAccessKey,
		cfg.S3Region,
		cfg.S3BucketName,
	)

	// Health check
	health := &handlers.HealthHandler{
		DB:         db,
		RDB:        rdb,
		IVSEnabled: ivsSvc.Enabled,
		S3Enabled:  s3Storage.Enabled,
		S3:         s3Storage,
		S3Missing:  cfg.MissingS3EnvKeys(),
	}
	app.Get("/health", health.Check)

	// Stripe webhook — before body parser; needs raw body intact
	stripeH := &handlers.StripeHandler{DB: db, Cfg: cfg, Email: emailSvc, IVS: ivsSvc}
	app.Post("/api/v1/webhooks/stripe", stripeH.Webhook)

	v1 := app.Group("/api/v1", middleware.RequireTrustedOrigin(trustedOrigins, sessionCfg.CookieName()))

	// Host dashboard access. The owner always passes; team members added
	// under Add User pass according to their role. Staff get Go Live, Chat,
	// Tickets/Flyer and Scan Tickets; admins get everything the owner has.
	hostAdmin := middleware.HostAccount(db, middleware.TeamAdmin)
	hostStaff := middleware.HostAccount(db, middleware.TeamAdmin, middleware.TeamStaff)
	// Staff only reach events assigned to them.
	eventAccess := middleware.RequireEventAccess(db)

	// Auth
	authH := &handlers.AuthHandler{DB: db, Cfg: cfg, Email: emailSvc, Session: sessionCfg}
	// Invite-only: register needs the token from a superuser's invite link.
	v1.Post("/auth/register", authH.Register)
	v1.Get("/auth/invitations/:token", authH.LookupInvitation)
	// Forgot password: emails a one-time link, then sets the new password.
	v1.Post("/auth/forgot-password", authH.ForgotPassword)
	v1.Get("/auth/password-resets/:token", authH.LookupPasswordReset)
	v1.Post("/auth/reset-password", authH.ResetPassword)
	v1.Post("/auth/login", authH.Login)
	v1.Post("/auth/logout", authH.Logout)
	v1.Post("/auth/logout-everywhere", requireSession, authH.LogoutEverywhere)
	v1.Get("/auth/me", requireSession, authH.Me)
	v1.Put("/auth/profile", requireSession, authH.UpdateProfile)
	v1.Post("/auth/change-password", requireSession, authH.ChangePassword)

	// Events
	eventH := &handlers.EventHandler{
		DB:  db,
		Cfg: cfg,
		IVS: ivsSvc,
		WiPay: &services.WiPayService{
			AccountNumber: cfg.WipayAccountNumber,
			CheckoutURL:   cfg.WipayCheckoutURL,
			CountryCode:   cfg.WipayCountryCode,
			Currency:      cfg.WipayCurrency,
			FeeStructure:  cfg.WipayFeeStructure,
			APIBaseURL:    cfg.WipayAPIBaseURL,
			APIKey:        cfg.WipayAPIKey,
			Environment:   cfg.WipayEnvironment,
		},
		PayPal: newPayPalService(cfg),
	}
	v1.Get("/pricing", eventH.Pricing)
	v1.Get("/events/public", eventH.ListPublic)
	v1.Get("/events/:id", eventH.GetByID)
	v1.Get("/events/:id/wipay/launch", eventH.WiPayLaunch)
	// Some hosted-checkout handoffs submit the launch URL as a form POST.
	// The signed state still authorizes the request in either form.
	v1.Post("/events/:id/wipay/launch", eventH.WiPayLaunch)
	v1.Post("/events", requireSession, hostAdmin, eventH.Create)
	v1.Get("/events", requireSession, hostStaff, eventH.ListByHost)
	v1.Post("/events/:id/checkout", requireSession, hostAdmin, eventH.Checkout)
	v1.Get("/events/:id/wipay/complete", eventH.WiPayComplete)
	// WiPay returns hosted-checkout results as a form POST. Keep GET as well
	// for browser redirects and manually opened return links.
	v1.Post("/events/:id/wipay/complete", eventH.WiPayComplete)
	v1.Get("/events/:id/paypal/complete", eventH.PayPalComplete)
	// Booking fee: Stripe sends the host back here after checkout.
	v1.Get("/events/:id/stripe/complete", eventH.StripeComplete)
	v1.Patch("/events/:id/ticket", requireSession, hostStaff, eventAccess, eventH.TicketSetup)
	v1.Post("/events/:id/bypass-activate", requireSession, hostAdmin, eventH.BypassActivate)
	v1.Delete("/events/:id", requireSession, hostAdmin, eventH.Delete)

	// Private S3-backed event images. Media reads are public because ticket
	// cards and flyers are public promotional assets; the bucket stays private.
	imageH := &handlers.ImageHandler{DB: db, Storage: s3Storage}
	v1.Get("/media/events/:id/:kind", imageH.Get)
	v1.Post("/events/:id/images/:kind", requireSession, hostStaff, eventAccess, imageH.Upload)
	v1.Delete("/events/:id/images/:kind", requireSession, hostStaff, eventAccess, imageH.Delete)

	// Advertisements
	adH := &handlers.AdvertisementHandler{DB: db}
	v1.Get("/advertisements", adH.ListPublic)
	v1.Get("/advertisements/mine", requireSession, hostStaff, adH.ListByHost)
	v1.Post("/advertisements", requireSession, hostStaff, adH.Create)
	v1.Put("/advertisements/:id", requireSession, hostStaff, adH.Update)
	v1.Delete("/advertisements/:id", requireSession, hostStaff, adH.Delete)

	// Stream credentials — host only, returns IVS ingest URL + stream key
	credH := &handlers.StreamCredentialsHandler{DB: db, IVS: ivsSvc, Playback: playback}
	v1.Get("/events/:id/stream-credentials", requireSession, hostStaff, eventAccess, credH.Get)
	v1.Post("/events/:id/reprovision-stream", requireSession, hostStaff, eventAccess, credH.Reprovision)
	// Live viewer count for the Go Live page
	v1.Get("/events/:id/viewers", requireSession, hostStaff, eventAccess, credH.Viewers)
	// Public — ticket holders poll this to know if the host is live right now
	v1.Get("/events/:id/stream-status", credH.Status)

	// Payouts — host onboarding across Stripe Connect, WiPay, and PayPal
	payoutAuth := middleware.RequirePayoutUnlock(cfg.JWTSecret)
	payoutSecurityH := &handlers.PayoutSecurityHandler{DB: db, Cfg: cfg}
	v1.Get("/connect/security/status", requireSession, hostAdmin, payoutSecurityH.Status)
	v1.Post("/connect/security/passcode", requireSession, hostAdmin, payoutSecurityH.Create)
	v1.Post("/connect/security/unlock", requireSession, hostAdmin, payoutSecurityH.Unlock)
	v1.Post("/connect/onboard", requireSession, hostAdmin, payoutAuth, stripeH.ConnectOnboard)
	payoutH := &handlers.PayoutHandler{
		DB:  db,
		Cfg: cfg,
		WiPay: &services.WiPayService{
			APIBaseURL:  cfg.WipayAPIBaseURL,
			APIKey:      cfg.WipayAPIKey,
			Environment: cfg.WipayEnvironment,
		},
		PayPal: newPayPalService(cfg),
	}
	v1.Get("/connect/status", requireSession, hostAdmin, payoutAuth, payoutH.Status)
	v1.Post("/connect/wipay", requireSession, hostAdmin, payoutAuth, payoutH.ConnectWiPay)
	v1.Post("/connect/paypal", requireSession, hostAdmin, payoutAuth, payoutH.ConnectPayPal)
	v1.Get("/connect/paypal/complete", payoutH.CompletePayPal)
	v1.Post("/connect/activate", requireSession, hostAdmin, payoutAuth, payoutH.Activate)
	v1.Post("/connect/deactivate", requireSession, hostAdmin, payoutAuth, payoutH.Deactivate)
	v1.Get("/connect/balance", requireSession, hostAdmin, payoutAuth, payoutH.Balance)
	v1.Post("/connect/payout", requireSession, hostAdmin, payoutAuth, payoutH.Payout)

	// Team — the Add User page: staff/admin logins on this host account
	teamH := &handlers.TeamHandler{DB: db}
	v1.Get("/team", requireSession, hostAdmin, teamH.List)
	v1.Post("/team", requireSession, hostAdmin, teamH.Create)
	v1.Patch("/team/:id", requireSession, hostAdmin, teamH.Update)
	v1.Delete("/team/:id", requireSession, hostAdmin, teamH.Delete)

	// Superuser — platform dashboard: analytics and control over every
	// user and event. Superusers are created with `go run ./cmd/superuser`.
	superH := &handlers.SuperuserHandler{DB: db, IVS: ivsSvc, Email: emailSvc}
	su := v1.Group("/superuser", requireSession, middleware.RequireSuperuser(db))
	su.Get("/overview", superH.Overview)
	su.Get("/users", superH.ListUsers)
	su.Patch("/users/:id", superH.UpdateUser)
	su.Delete("/users/:id", superH.RejectUser)
	su.Get("/invitations", superH.ListInvitations)
	su.Post("/invitations", superH.CreateInvitation)
	su.Post("/invitations/:id/resend", superH.ResendInvitation)
	su.Delete("/invitations/:id", superH.DeleteInvitation)
	su.Get("/events", superH.ListEvents)
	su.Patch("/events/:id", superH.UpdateEvent)
	su.Post("/events/:id/activate", superH.ActivateEvent)
	su.Post("/events/:id/cancel", superH.CancelEvent)

	// Tickets
	ticketH := &handlers.TicketHandler{
		DB: db, Cfg: cfg, Email: emailSvc,
		PayPal:   newPayPalService(cfg),
		Playback: playback,
	}
	app.Post("/api/v1/webhooks/paypal", ticketH.PayPalWebhook)
	// Find my tickets: emails the tickets to the address (never returns them).
	// The old GET ?email= form leaked tickets and is answered with 410.
	v1.Post("/tickets/lookup", ticketH.LookupByEmail)
	v1.Get("/tickets/lookup", ticketH.LookupGone)
	// One-time claim link from the ticket-success page.
	v1.Get("/tickets/claim/:token", ticketH.Claim)
	v1.Get("/tickets/enter", ticketH.Enter)
	v1.Post("/tickets/guest-purchase", ticketH.GuestPurchase)
	v1.Get("/tickets/paypal/complete", ticketH.PayPalComplete)
	v1.Post("/tickets/purchase", requireSession, ticketH.Purchase)
	v1.Get("/tickets/mine", requireSession, ticketH.ListMine)
	// Door-scanner check-in — host only, used by the dashboard's Scan Tickets page
	v1.Post("/tickets/checkin", requireSession, hostStaff, ticketH.CheckIn)

	// Viewer stream — Redis session locking
	guard := &services.SessionGuard{RDB: rdb}
	streamH := &handlers.StreamHandler{DB: db, Guard: guard, Playback: playback}
	v1.Post("/stream/watch", requireSession, streamH.Watch)
	v1.Post("/stream/heartbeat", requireSession, streamH.Heartbeat)
	v1.Post("/stream/release", requireSession, streamH.Release)

	// Live chat — ticket-gated registration, Redis-backed history/mutes,
	// in-process WebSocket fan-out
	chatH := &handlers.ChatHandler{Hub: handlers.NewChatHub(), DB: db, Cfg: cfg, RDB: rdb, Session: sessionCfg}
	v1.Post("/events/:id/chat/register", chatH.Register)
	v1.Use("/events/:id/chat/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	v1.Get("/events/:id/chat/ws", websocket.New(chatH.HandleWS))
}

// newPayPalService returns an unconfigured (disabled) client unless
// PAYPAL_ENABLED=true, so every PayPal flow refuses by default.
func newPayPalService(cfg *config.Config) *services.PayPalService {
	if !cfg.PaypalEnabled {
		return &services.PayPalService{}
	}
	return &services.PayPalService{
		ClientID: cfg.PaypalClientID, ClientSecret: cfg.PaypalClientSecret, Environment: cfg.PaypalEnvironment,
		PartnerMerchantID: cfg.PaypalPartnerMerchantID, PartnerAttributionID: cfg.PaypalPartnerAttributionID,
		WebhookID: cfg.PaypalWebhookID,
	}
}
