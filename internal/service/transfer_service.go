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
	if err := validateAmountFitsSchema(i.Amount); err != nil {
		return err
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
	ErrInvalidInput         = errors.New("invalid input")
	ErrWalletNotFound       = errors.New("wallet not found")
	ErrIdempotencyConflict  = errors.New("idempotency key conflicts with existing transfer")
	ErrTransferStillPending = errors.New("transfer is still pending")
	ErrSameWallet           = errors.New("cannot transfer to the same wallet")
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
		if err := ensureIdempotentReplayMatches(existing, req); err != nil {
			return nil, err
		}
		if existing.Status == domain.StatusPending {
			return nil, ErrTransferStillPending
		}
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
			existing, err = s.waitForExistingTransferToSettle(ctx, req, existing)
			if err != nil {
				return nil, err
			}
			return toResponse(existing, true), nil
		}
		if repository.IsForeignKeyViolation(err) {
			return nil, ErrWalletNotFound
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
		if errors.Is(err, repository.ErrWalletNotFound) {
			err = ErrWalletNotFound
		}
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
		if errors.Is(err, repository.ErrWalletNotFound) {
			return nil, nil, ErrWalletNotFound
		}
		return nil, nil, err
	}

	secondWallet, err := s.walletRepo.LockByID(ctx, secondID)
	if err != nil {
		if errors.Is(err, repository.ErrWalletNotFound) {
			return nil, nil, ErrWalletNotFound
		}
		return nil, nil, err
	}

	if firstWallet.ID == fromID {
		return firstWallet, secondWallet, nil
	}
	return secondWallet, firstWallet, nil
}

func ensureIdempotentReplayMatches(existing *domain.Transfer, req CreateTransferRequest) error {
	if existing.FromWalletID != req.FromWalletID ||
		existing.ToWalletID != req.ToWalletID ||
		!existing.Amount.Equal(req.Amount) {
		return ErrIdempotencyConflict
	}
	return nil
}

func validateAmountFitsSchema(amount decimal.Decimal) error {
	const maxScale = 8
	const maxIntegerDigits = 12

	scale := int32(0)
	if amount.Exponent() < 0 {
		scale = -amount.Exponent()
	}
	if scale > maxScale {
		return fmt.Errorf("amount supports at most %d decimal places", maxScale)
	}

	coefficientDigits := len(amount.Abs().Coefficient().String())
	integerDigits := coefficientDigits
	if scale > 0 {
		integerDigits -= int(scale)
		if integerDigits < 0 {
			integerDigits = 0
		}
	} else if amount.Exponent() > 0 {
		integerDigits += int(amount.Exponent())
	}
	if integerDigits > maxIntegerDigits {
		return fmt.Errorf("amount supports at most %d digits before decimal point", maxIntegerDigits)
	}

	return nil
}

func (s *TransferService) waitForExistingTransferToSettle(
	ctx context.Context,
	req CreateTransferRequest,
	existing *domain.Transfer,
) (*domain.Transfer, error) {
	const attempts = 10
	const delay = 10 * time.Millisecond

	for i := 0; i < attempts; i++ {
		if err := ensureIdempotentReplayMatches(existing, req); err != nil {
			return nil, err
		}
		if existing.Status != domain.StatusPending {
			return existing, nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		var err error
		existing, err = s.transferRepo.FindByIdempotencyKey(ctx, req.IdempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("find existing transfer while waiting for pending transfer: %w", err)
		}
		if existing == nil {
			return nil, fmt.Errorf("transfer already exists but could not be found")
		}
	}

	if err := ensureIdempotentReplayMatches(existing, req); err != nil {
		return nil, err
	}
	return nil, ErrTransferStillPending
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
