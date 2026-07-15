package main

import (
	"log"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/joho/godotenv"

	"vertualeventlive/backend/config"
	"vertualeventlive/backend/database"
	"vertualeventlive/backend/routes"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file — using system environment")
	}

	cfg := config.Load()

	db, err := database.ConnectPostgres(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("PostgreSQL: %v", err)
	}
	defer db.Close()

	if err := database.RunMigrations(db); err != nil {
		log.Fatalf("Migrations: %v", err)
	}
	log.Println("Migrations OK")

	rdb := database.ConnectRedis(cfg.RedisURL)
	defer rdb.Close()

	app := fiber.New(fiber.Config{
		AppName:      "VirtualEventLive API v1",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	})

	app.Use(recover.New())
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins:     cfg.FrontendURL + ",http://localhost:5173",
		AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
		AllowHeaders:     "Origin, Content-Type, Accept, Authorization",
		AllowCredentials: true,
	}))

	routes.Register(app, db, rdb, cfg)

	log.Printf("Server listening on :%s", cfg.Port)
	log.Fatal(app.Listen(":" + cfg.Port))
}
