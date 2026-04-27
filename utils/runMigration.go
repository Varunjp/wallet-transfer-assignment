package utils

import (
	"context"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

func RunMigrations(db *pgxpool.Pool) {
	ctx := context.Background()
	sqlBytes, err := os.ReadFile("migrations/001_init.sql")
	if err != nil {
		log.Fatalf("read migration: %v", err)
	}

	_, err = db.Exec(ctx, string(sqlBytes))
	if err != nil {
		log.Fatalf("run migration: %v", err)
	}

	log.Println("Migrations applied")
}