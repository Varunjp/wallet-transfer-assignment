package service

import (
	"context"
	"wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type TransferRepository interface {
    Create(ctx context.Context, t *domain.Transfer) error
    FindByIdempotencyKey(ctx context.Context, key string) (*domain.Transfer, error)
    UpdateStatus(ctx context.Context, id uuid.UUID, status domain.TransferStatus, reason *string) error
}

type WalletRepository interface {
    FindByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)
}

type TxManager interface {
    WithTx(ctx context.Context, fn func(txCtx context.Context) error) error
}

type WalletTxRepository interface {
    LockByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error)
    ApplyDebitCredit(ctx context.Context, fromID, toID uuid.UUID, amount decimal.Decimal) error
}

type LedgerTxRepository interface {
    BulkInsert(ctx context.Context, entries []domain.LedgerEntry) error
}