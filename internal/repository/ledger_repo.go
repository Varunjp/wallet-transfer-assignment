package repository

import (
	"context"
	"fmt"
	"wallet-service/internal/domain"

	"github.com/jackc/pgx/v5"
)

type LedgerRepository struct{}

func NewLedgerRepository() *LedgerRepository {
	return &LedgerRepository{}
}

func (r *LedgerRepository) BulkInsert(ctx context.Context, entries []domain.LedgerEntry) error {
	tx, err := getTx(ctx)
	if err != nil {
		return err
	}

	if len(entries) != 2 {
		return fmt.Errorf("ledger must have 2 entries")
	}

	rows := make([][]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{e.ID, e.WalletID, e.TransferID, string(e.Type), e.Amount}
	}

	_, err = tx.CopyFrom(
		ctx,
		pgx.Identifier{"ledger_entries"},
		[]string{"id", "wallet_id", "transfer_id", "type", "amount"},
		pgx.CopyFromRows(rows),
	)

	return err
}
