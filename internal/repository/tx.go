package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type txKey struct{}

type TxManager struct {
	db *pgxpool.Pool
}

func NewTxManger(db *pgxpool.Pool) *TxManager {
	return &TxManager{db: db}
}

func (m *TxManager) WithTx(ctx context.Context,fn func(ctx context.Context)error) error {
	tx,err := m.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w",err)
	}

	txCtx := context.WithValue(ctx,txKey{},tx)

	if err := fn(txCtx); err != nil {
		_ = tx.Rollback(ctx)
		return err 
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w",err)
	}

	return nil 
}

func getTx(ctx context.Context) pgx.Tx {
	tx, _ := ctx.Value(txKey{}).(pgx.Tx)
	return tx
}