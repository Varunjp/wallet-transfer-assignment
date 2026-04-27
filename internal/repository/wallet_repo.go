package repository

import (
	"context"
	"errors"
	"fmt"
	"wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

type WalletRepository struct {
	db *pgxpool.Pool
}

func NewWalletRepository(db *pgxpool.Pool) *WalletRepository {
	return &WalletRepository{db: db}
}

func (r *WalletRepository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	row := r.db.QueryRow(ctx, `
		SELECT id, owner_id, balance, created_at, updated_at
		FROM wallets WHERE id = $1
	`, id)

	return scanWallet(row)
}

func (r *WalletRepository) LockByID(ctx context.Context, id uuid.UUID) (*domain.Wallet, error) {
	tx, err := getTx(ctx)
	if err != nil {
		return nil, err
	}

	row := tx.QueryRow(ctx, `
		SELECT id, owner_id, balance, created_at, updated_at
		FROM wallets
		WHERE id = $1
		FOR UPDATE
	`, id)

	return scanWallet(row)
}

func (r *WalletRepository) ApplyDebitCredit(
	ctx context.Context,
	fromID, toID uuid.UUID,
	amount decimal.Decimal,
) error {
	tx, err := getTx(ctx)
	if err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE wallets
		SET balance = balance - $1, updated_at = NOW()
		WHERE id = $2
	`, amount, fromID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWalletNotFound
	}

	tag, err = tx.Exec(ctx, `
		UPDATE wallets
		SET balance = balance + $1, updated_at = NOW()
		WHERE id = $2
	`, amount, toID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrWalletNotFound
	}

	return nil
}

func scanWallet(row interface{ Scan(...any) error }) (*domain.Wallet, error) {
	w := &domain.Wallet{}
	err := row.Scan(&w.ID, &w.OwnerID, &w.Balance, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrWalletNotFound
		}
		return nil, fmt.Errorf("scan wallet: %w", err)
	}
	return w, nil
}
