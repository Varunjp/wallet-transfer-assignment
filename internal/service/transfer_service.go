package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"wallet-service/internal/domain"
	"wallet-service/internal/repository"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// ── Input / Output types ─────────────────────────────────────────────────────
type CreateTransferRequest struct {
	IdempotencyKey string
	FromWalletID   uuid.UUID
	ToWalletID     uuid.UUID
	Amount         decimal.Decimal
}

// validate checks all business rules before any DB interaction.
func (i CreateTransferRequest) validate() error {
	if i.IdempotencyKey == "" {
		return fmt.Errorf("idempotency key is required")
	}
	if i.FromWalletID == uuid.Nil {
		return fmt.Errorf("from wallet id is required")
	}
	if i.ToWalletID == uuid.Nil {
		return fmt.Errorf("to wallet id is required")
	}
	if i.FromWalletID == i.ToWalletID {
		return fmt.Errorf("from and to wallets must be different")
	}
	if i.Amount.LessThanOrEqual(decimal.Zero) {
		return fmt.Errorf("amount must be greater than zero")
	}
	return nil
}

type TransferResponse struct {
	ID             uuid.UUID             `json:"id"`
	IdempotencyKey string                `json:"idempotencyKey"`
	FromWalletID   uuid.UUID             `json:"fromWalletId"`
	ToWalletID     uuid.UUID             `json:"toWalletId"`
	Amount         decimal.Decimal       `json:"amount"`
	Status         domain.TransferStatus `json:"status"`
	CreatedAt      time.Time             `json:"createdAt"`
	Replayed       bool                  `json:"replayed,omitempty"`
}

// ── Service errors ────────────────────────────────────────────────────────────
// Define sentinel errors here so handlers can errors.Is() against them
// without importing the domain or repository packages.

var (
	ErrInvalidInput   = errors.New("invalid input")
	ErrWalletNotFound = errors.New("wallet not found")
	ErrSameWallet     = errors.New("cannot transfer to the same wallet")
)

type TransferService struct {
	txManager    TxManager
	transferRepo TransferRepository
	walletRepo   WalletTxRepository
	ledgerRepo   LedgerTxRepository
	log          *slog.Logger
}

func NewTransferService(
	txManager TxManager,
	transferRepo TransferRepository,
	walletRepo WalletTxRepository,
	ledgerRepo LedgerTxRepository,
	log *slog.Logger,
) *TransferService {
	return &TransferService{
		txManager:    txManager,
		transferRepo: transferRepo,
		walletRepo:   walletRepo,
		ledgerRepo:   ledgerRepo,
		log:          log,
	}
}

func (s *TransferService) CreateTransfer(ctx context.Context, req CreateTransferRequest) (*TransferResponse, error) {

	// ── Step 1: validate input before touching the database ──────────────────
	// Cheap, no IO. Catches bad requests immediately.
	if err := req.validate(); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err)
	}

	existing, err := s.transferRepo.FindByIdempotencyKey(ctx, req.IdempotencyKey)
	if err != nil {
		s.log.Warn("idempotency pre-check failed, continuing", "key", req.IdempotencyKey, "error", err)
	}

	if existing != nil {
		s.log.Info("idempotent request hit",
			"key", req.IdempotencyKey,
			"transfer_id", existing.ID,
		)
		return toResponse(existing, true), nil
	}

	transfer := &domain.Transfer{
		ID:             uuid.New(),
		IdempotencyKey: req.IdempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
		Status:         domain.StatusPending,
	}

	err = s.transferRepo.Create(ctx, transfer)

	if err != nil {
		if repository.IsUniqueViolation(err) {
			existing, lookupErr := s.transferRepo.FindByIdempotencyKey(ctx, req.IdempotencyKey)
			if lookupErr != nil {
				return nil, fmt.Errorf("find existing transfer after unique violation: %w", lookupErr)
			}
			if existing == nil {
				return nil, fmt.Errorf("transfer already exists but could not be found")
			}
			return toResponse(existing, true), nil
		}
		return nil, err
	}

	s.log.Info("create transfer request",
		"idempotencyKey", req.IdempotencyKey,
		"fromWallet", req.FromWalletID,
		"toWallet", req.ToWalletID,
		"amount", req.Amount,
	)

	err = s.txManager.WithTx(ctx, func(txCtx context.Context) error {
		s.log.Debug("starting transfer transaction",
			"transferID", transfer.ID,
		)

		fromWallet, toWallet, err := s.lockTransferWallets(txCtx, req.FromWalletID, req.ToWalletID)
		if err != nil {
			return err
		}

		if err := fromWallet.ValidateDebit(req.Amount); err != nil {
			return err
		}

		err = s.walletRepo.ApplyDebitCredit(
			txCtx,
			fromWallet.ID,
			toWallet.ID,
			req.Amount,
		)
		if err != nil {
			return err
		}

		entries := []domain.LedgerEntry{
			{
				ID:         uuid.New(),
				WalletID:   fromWallet.ID,
				TransferID: transfer.ID,
				Type:       domain.EntryDebit,
				Amount:     req.Amount,
			},
			{
				ID:         uuid.New(),
				WalletID:   toWallet.ID,
				TransferID: transfer.ID,
				Type:       domain.EntryCredit,
				Amount:     req.Amount,
			},
		}

		if err := s.ledgerRepo.BulkInsert(txCtx, entries); err != nil {
			return err
		}

		return s.transferRepo.Transition(
			txCtx,
			transfer.ID,
			domain.StatusPending,
			domain.StatusProcessed,
		)
	})

	if err != nil {
		reason := err.Error()
		_ = s.transferRepo.UpdateStatus(
			ctx,
			transfer.ID,
			domain.StatusFailed,
			&reason,
		)
		s.log.Error("transfer failed",
			"transferID", transfer.ID,
			"error", err,
		)
		return nil, err
	}

	transfer.Status = domain.StatusProcessed
	s.log.Info("transfer completed",
		"transferID", transfer.ID,
		"status", "PROCESSED",
	)
	return toResponse(transfer, false), nil
}

func (s *TransferService) lockTransferWallets(ctx context.Context, fromID, toID uuid.UUID) (*domain.Wallet, *domain.Wallet, error) {
	firstID, secondID := fromID, toID
	if bytes.Compare(fromID[:], toID[:]) > 0 {
		firstID, secondID = toID, fromID
	}

	firstWallet, err := s.walletRepo.LockByID(ctx, firstID)
	if err != nil {
		return nil, nil, err
	}

	secondWallet, err := s.walletRepo.LockByID(ctx, secondID)
	if err != nil {
		return nil, nil, err
	}

	if firstWallet.ID == fromID {
		return firstWallet, secondWallet, nil
	}
	return secondWallet, firstWallet, nil
}

func toResponse(t *domain.Transfer, replayed bool) *TransferResponse {
	return &TransferResponse{
		ID:             t.ID,
		IdempotencyKey: t.IdempotencyKey,
		FromWalletID:   t.FromWalletID,
		ToWalletID:     t.ToWalletID,
		Amount:         t.Amount,
		Status:         t.Status,
		CreatedAt:      t.CreatedAt,
		Replayed:       replayed,
	}
}
