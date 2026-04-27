package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"wallet-service/internal/config"
	"wallet-service/internal/db"
	"wallet-service/internal/handler"
	"wallet-service/internal/repository"
	"wallet-service/internal/service"
	"wallet-service/utils"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func main() {

	ctx := context.Background()

	cfg,err := config.Load()

	if err != nil {
		log.Fatalf("failed to load env : %v",err)
	}

	db,err := db.NewPool(ctx,cfg.DB)
	if err != nil {
		log.Fatalf("failed to connect db: %v",err)
	}
	defer db.Close()

	utils.RunMigrations(db)

	txManager := repository.NewTxManager(db)
	transferRepo := repository.NewTransferRepository(db)
	walletRepo := repository.NewWalletRepository(db)
	ledgerRepo := repository.NewLedgerRepository()

	logg := slog.New(slog.NewJSONHandler(os.Stdout,nil))

	transferService := service.NewTransferService(
		txManager,
		transferRepo,
		walletRepo,
		ledgerRepo,
		logg,
	)

	transferHandler := handler.NewTransferHandler(transferService)

	seedData(ctx,db)

	r := gin.Default()
	handler.RegisterRoutes(r,transferHandler)

	log.Printf("Server running on :%s\n",cfg.PORT)

	srv := &http.Server{
		Addr: ":"+cfg.PORT,
		Handler: r,
	}

	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}


func seedData(ctx context.Context, db *pgxpool.Pool) {
	log.Println("Seeding data...")

	wallets := []struct {
		id      string
		owner   string
		balance decimal.Decimal
	}{
		{"11111111-1111-1111-1111-111111111111", "user3", decimal.NewFromInt(1000)},
		{"22222222-2222-2222-2222-222222222222", "user4", decimal.NewFromInt(500)},
	}

	for _, w := range wallets {
		_, err := db.Exec(ctx, `
			INSERT INTO wallets (id, owner_id, balance)
			VALUES ($1, $2, $3)
			ON CONFLICT (id) DO NOTHING
		`, w.id, w.owner, w.balance)

		if err != nil {
			log.Printf("seed wallet error: %v", err)
		} else {
			log.Printf("Wallet created: %s (%s)", w.id, w.owner)
		}
	}
}