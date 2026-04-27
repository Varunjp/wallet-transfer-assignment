package repository

import (
	"context"
	"errors"
	"fmt"
	"wallet-service/internal/domain"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TransferRepository struct {
	db *pgxpool.Pool
}

func NewTransferRepository(db *pgxpool.Pool) *TransferRepository {
	return &TransferRepository{db: db}
}

func (r *TransferRepository) Create(ctx context.Context, t *domain.Transfer) error {
	query := `
		INSERT INTO transfers (id, idempotency_key, from_wallet_id, to_wallet_id, amount, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at
	`
	err := r.db.QueryRow(
		ctx,
		query,
		t.ID,
		t.IdempotencyKey,
		t.FromWalletID,
		t.ToWalletID,
		t.Amount,
		t.Status,
	).Scan(&t.CreatedAt, &t.UpdatedAt)

	return err
}

func (r *TransferRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status domain.TransferStatus, reason *string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE transfers
		SET status = $1,
		    failure_reason = $2,
		    updated_at = NOW()
		WHERE id = $3
	`, status, reason, id)

	return err
}

func (r *TransferRepository) Transition(
	ctx context.Context,
	id uuid.UUID,
	from, to domain.TransferStatus,
) error {
	tx, err := getTx(ctx)
	if err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		UPDATE transfers
		SET status = $1,
		    updated_at = NOW()
		WHERE id = $2 AND status = $3
	`, to, id, from)

	if err != nil {
		return fmt.Errorf("transition transfer: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invalid state transition")
	}

	return nil
}

func (r *TransferRepository) FindByIdempotencyKey(ctx context.Context, key string) (*domain.Transfer, error) {
	row := r.db.QueryRow(ctx, `
		SELECT id,idempotency_key,from_wallet_id, to_wallet_id,
		       amount, status, failure_reason, created_at, updated_at
		FROM transfers
		WHERE idempotency_key = $1
	`, key)

	t, err := scanTransfer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

func scanTransfer(row interface {
	Scan(...any) error
}) (*domain.Transfer, error) {
	t := &domain.Transfer{}
	err := row.Scan(
		&t.ID, &t.IdempotencyKey,
		&t.FromWalletID, &t.ToWalletID,
		&t.Amount, &t.Status, &t.FailureReason,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("scan transfer: %w", err)
	}
	return t, nil
}
