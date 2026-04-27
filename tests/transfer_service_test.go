package tests

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
	"wallet-service/internal/domain"
	"wallet-service/internal/repository"
	"wallet-service/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

func TestCreateTransferValidationErrors(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()

	tests := []struct {
		name string
		req  service.CreateTransferRequest
	}{
		{
			name: "missing idempotency key",
			req: service.CreateTransferRequest{
				FromWalletID: fromID,
				ToWalletID:   toID,
				Amount:       decimal.NewFromInt(10),
			},
		},
		{
			name: "missing from wallet",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				ToWalletID:     toID,
				Amount:         decimal.NewFromInt(10),
			},
		},
		{
			name: "missing to wallet",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				FromWalletID:   fromID,
				Amount:         decimal.NewFromInt(10),
			},
		},
		{
			name: "same wallet",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				FromWalletID:   fromID,
				ToWalletID:     fromID,
				Amount:         decimal.NewFromInt(10),
			},
		},
		{
			name: "zero amount",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				FromWalletID:   fromID,
				ToWalletID:     toID,
				Amount:         decimal.Zero,
			},
		},
		{
			name: "negative amount",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				FromWalletID:   fromID,
				ToWalletID:     toID,
				Amount:         decimal.NewFromInt(-1),
			},
		},
		{
			name: "too many decimal places",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				FromWalletID:   fromID,
				ToWalletID:     toID,
				Amount:         decimal.RequireFromString("1.123456789"),
			},
		},
		{
			name: "too many integer digits",
			req: service.CreateTransferRequest{
				IdempotencyKey: "key-1",
				FromWalletID:   fromID,
				ToWalletID:     toID,
				Amount:         decimal.RequireFromString("1000000000000"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transferRepo := &fakeTransferRepo{}
			txManager := &fakeTxManager{}
			svc := newTestTransferService(txManager, transferRepo, &fakeWalletRepo{}, &fakeLedgerRepo{})

			resp, err := svc.CreateTransfer(context.Background(), tt.req)
			if resp != nil {
				t.Fatalf("expected nil response, got %#v", resp)
			}
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput, got %v", err)
			}
			if transferRepo.findCalls != 0 || transferRepo.createCalls != 0 || txManager.calls != 0 {
				t.Fatalf("validation should happen before IO, got find=%d create=%d tx=%d",
					transferRepo.findCalls, transferRepo.createCalls, txManager.calls)
			}
		})
	}
}

func TestCreateTransferRejectsIdempotencyKeyWithDifferentPayload(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	existing := &domain.Transfer{
		ID:             uuid.New(),
		IdempotencyKey: "repeat-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(25),
		Status:         domain.StatusProcessed,
	}
	transferRepo := &fakeTransferRepo{existing: existing}
	txManager := &fakeTxManager{}
	svc := newTestTransferService(txManager, transferRepo, &fakeWalletRepo{}, &fakeLedgerRepo{})

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "repeat-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(30),
	})

	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
	if !errors.Is(err, service.ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}
	if transferRepo.createCalls != 0 || txManager.calls != 0 {
		t.Fatalf("conflicting replay should skip create and tx, got create=%d tx=%d",
			transferRepo.createCalls, txManager.calls)
	}
}

func TestCreateTransferResumesPendingIdempotencyReplay(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	existing := &domain.Transfer{
		ID:             uuid.New(),
		IdempotencyKey: "pending-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(25),
		Status:         domain.StatusPending,
	}
	transferRepo := &fakeTransferRepo{existing: existing}
	txManager := &fakeTxManager{}
	walletRepo := &fakeWalletRepo{
		wallets: map[uuid.UUID]*domain.Wallet{
			fromID: {ID: fromID, Balance: decimal.NewFromInt(50)},
			toID:   {ID: toID, Balance: decimal.NewFromInt(10)},
		},
	}
	ledgerRepo := &fakeLedgerRepo{}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "pending-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(25),
	})

	if err != nil {
		t.Fatalf("expected pending transfer to be resumed, got %v", err)
	}
	if resp == nil || !resp.Replayed || resp.Status != domain.StatusProcessed {
		t.Fatalf("expected replayed processed response, got %#v", resp)
	}
	if transferRepo.createCalls != 0 || txManager.calls != 1 || walletRepo.applyCalls != 1 || len(ledgerRepo.entries) != 2 {
		t.Fatalf("pending replay should process once, create=%d tx=%d apply=%d ledger=%d",
			transferRepo.createCalls, txManager.calls, walletRepo.applyCalls, len(ledgerRepo.entries))
	}
}

func TestCreateTransferReturnsExistingTransferForIdempotencyHit(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	existing := &domain.Transfer{
		ID:             uuid.New(),
		IdempotencyKey: "repeat-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(25),
		Status:         domain.StatusProcessed,
		CreatedAt:      time.Now(),
	}
	transferRepo := &fakeTransferRepo{existing: existing}
	txManager := &fakeTxManager{}
	svc := newTestTransferService(txManager, transferRepo, &fakeWalletRepo{}, &fakeLedgerRepo{})

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "repeat-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(25),
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resp == nil || !resp.Replayed {
		t.Fatalf("expected replayed response, got %#v", resp)
	}
	if resp.ID != existing.ID || resp.Status != domain.StatusProcessed {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if transferRepo.createCalls != 0 || txManager.calls != 0 {
		t.Fatalf("idempotency hit should skip create and tx, got create=%d tx=%d",
			transferRepo.createCalls, txManager.calls)
	}
}

func TestCreateTransferProcessesWalletTransferAndLedgerEntries(t *testing.T) {
	fromID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	toID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	amount := decimal.NewFromInt(75)
	transferRepo := &fakeTransferRepo{}
	txManager := &fakeTxManager{}
	walletRepo := &fakeWalletRepo{
		wallets: map[uuid.UUID]*domain.Wallet{
			fromID: {ID: fromID, Balance: decimal.NewFromInt(100)},
			toID:   {ID: toID, Balance: decimal.NewFromInt(20)},
		},
	}
	ledgerRepo := &fakeLedgerRepo{}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "key-success",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         amount,
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resp == nil || resp.Replayed || resp.Status != domain.StatusProcessed {
		t.Fatalf("expected processed non-replayed response, got %#v", resp)
	}
	if !resp.Amount.Equal(amount) || resp.FromWalletID != fromID || resp.ToWalletID != toID {
		t.Fatalf("response does not match request: %#v", resp)
	}
	if transferRepo.createCalls != 1 || txManager.calls != 1 || transferRepo.transitionCalls != 1 {
		t.Fatalf("unexpected call counts create=%d tx=%d transition=%d",
			transferRepo.createCalls, txManager.calls, transferRepo.transitionCalls)
	}
	if len(walletRepo.lockOrder) != 2 || walletRepo.lockOrder[0] != toID || walletRepo.lockOrder[1] != fromID {
		t.Fatalf("wallet locks should be acquired in UUID order, got %v", walletRepo.lockOrder)
	}
	if walletRepo.debitFrom != fromID || walletRepo.creditTo != toID || !walletRepo.appliedAmount.Equal(amount) {
		t.Fatalf("unexpected debit/credit call from=%s to=%s amount=%s",
			walletRepo.debitFrom, walletRepo.creditTo, walletRepo.appliedAmount)
	}
	if len(ledgerRepo.entries) != 2 {
		t.Fatalf("expected 2 ledger entries, got %d", len(ledgerRepo.entries))
	}
	if ledgerRepo.entries[0].WalletID != fromID || ledgerRepo.entries[0].Type != domain.EntryDebit {
		t.Fatalf("unexpected debit ledger entry: %#v", ledgerRepo.entries[0])
	}
	if ledgerRepo.entries[1].WalletID != toID || ledgerRepo.entries[1].Type != domain.EntryCredit {
		t.Fatalf("unexpected credit ledger entry: %#v", ledgerRepo.entries[1])
	}
}

func TestCreateTransferMarksTransferFailedWhenFundsAreInsufficient(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	transferRepo := &fakeTransferRepo{}
	txManager := &fakeTxManager{}
	walletRepo := &fakeWalletRepo{
		wallets: map[uuid.UUID]*domain.Wallet{
			fromID: {ID: fromID, Balance: decimal.NewFromInt(10)},
			toID:   {ID: toID, Balance: decimal.NewFromInt(100)},
		},
	}
	ledgerRepo := &fakeLedgerRepo{}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "key-insufficient",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(50),
	})

	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
	if !errors.Is(err, domain.ErrInsufficientFunds) {
		t.Fatalf("expected insufficient funds, got %v", err)
	}
	if transferRepo.updateStatusCalls != 1 || transferRepo.updatedStatus != domain.StatusFailed {
		t.Fatalf("expected transfer to be marked failed, calls=%d status=%s",
			transferRepo.updateStatusCalls, transferRepo.updatedStatus)
	}
	if transferRepo.failureReason == nil || *transferRepo.failureReason != domain.ErrInsufficientFunds.Error() {
		t.Fatalf("unexpected failure reason: %#v", transferRepo.failureReason)
	}
	if walletRepo.applyCalls != 0 || len(ledgerRepo.entries) != 0 || transferRepo.transitionCalls != 0 {
		t.Fatalf("failed debit should skip apply, ledger, and transition")
	}
}

func TestCreateTransferMarksFailedWhenWalletIsMissing(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	transferRepo := &fakeTransferRepo{}
	txManager := &fakeTxManager{}
	walletRepo := &fakeWalletRepo{
		wallets: map[uuid.UUID]*domain.Wallet{
			fromID: {ID: fromID, Balance: decimal.NewFromInt(100)},
		},
	}
	ledgerRepo := &fakeLedgerRepo{}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "key-missing-wallet",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(50),
	})

	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
	if !errors.Is(err, service.ErrWalletNotFound) {
		t.Fatalf("expected wallet not found, got %v", err)
	}
	if transferRepo.updateStatusCalls != 1 || transferRepo.updatedStatus != domain.StatusFailed {
		t.Fatalf("expected transfer to be marked failed, calls=%d status=%s",
			transferRepo.updateStatusCalls, transferRepo.updatedStatus)
	}
	if walletRepo.applyCalls != 0 || len(ledgerRepo.entries) != 0 || transferRepo.transitionCalls != 0 {
		t.Fatalf("missing wallet should skip apply, ledger, and transition")
	}
}

func TestCreateTransferReturnsExistingTransferAfterUniqueViolation(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	existing := &domain.Transfer{
		ID:             uuid.New(),
		IdempotencyKey: "race-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(15),
		Status:         domain.StatusProcessed,
	}
	transferRepo := &fakeTransferRepo{
		createErr:              &pgconn.PgError{Code: "23505"},
		existingAfterCreateErr: existing,
	}
	txManager := &fakeTxManager{}
	svc := newTestTransferService(txManager, transferRepo, &fakeWalletRepo{}, &fakeLedgerRepo{})

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "race-key",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(15),
	})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if resp == nil || !resp.Replayed || resp.ID != existing.ID {
		t.Fatalf("expected replayed existing transfer, got %#v", resp)
	}
	if transferRepo.findCalls != 2 || txManager.calls != 0 {
		t.Fatalf("expected second lookup and no tx, got find=%d tx=%d", transferRepo.findCalls, txManager.calls)
	}
}

func TestCreateTransferConcurrentRequestsWithSameIdempotencyKey(t *testing.T) {
	const requestCount = 20

	fromID := uuid.New()
	toID := uuid.New()
	amount := decimal.NewFromInt(10)
	transferRepo := newConcurrentTransferRepo(requestCount)
	txManager := &concurrentTxManager{}
	walletRepo := newConcurrentWalletRepo(map[uuid.UUID]*domain.Wallet{
		fromID: {ID: fromID, Balance: decimal.NewFromInt(100)},
		toID:   {ID: toID, Balance: decimal.NewFromInt(25)},
	})
	ledgerRepo := &concurrentLedgerRepo{}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	start := make(chan struct{})
	var wg sync.WaitGroup
	responses := make(chan *service.TransferResponse, requestCount)
	errorsCh := make(chan error, requestCount)

	for range requestCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
				IdempotencyKey: "same-concurrent-key",
				FromWalletID:   fromID,
				ToWalletID:     toID,
				Amount:         amount,
			})
			responses <- resp
			errorsCh <- err
		}()
	}

	close(start)
	wg.Wait()
	close(responses)
	close(errorsCh)

	for err := range errorsCh {
		if err != nil {
			t.Fatalf("expected all concurrent requests to succeed, got %v", err)
		}
	}

	var transferID uuid.UUID
	replayedCount := 0
	responseCount := 0
	for resp := range responses {
		if resp == nil {
			t.Fatal("expected response, got nil")
		}
		if transferID == uuid.Nil {
			transferID = resp.ID
		}
		if resp.ID != transferID {
			t.Fatalf("expected all responses to return transfer %s, got %s", transferID, resp.ID)
		}
		if resp.Replayed {
			replayedCount++
		}
		responseCount++
	}

	if responseCount != requestCount {
		t.Fatalf("expected %d responses, got %d", requestCount, responseCount)
	}
	if replayedCount != requestCount-1 {
		t.Fatalf("expected %d replayed responses, got %d", requestCount-1, replayedCount)
	}
	if transferRepo.createCalls() != requestCount {
		t.Fatalf("expected each request to attempt create after concurrent pre-check, got %d", transferRepo.createCalls())
	}
	if txManager.callCount() != 1 {
		t.Fatalf("expected only one transaction to process wallets, got %d", txManager.callCount())
	}
	if walletRepo.applyCallCount() != 1 {
		t.Fatalf("expected only one debit/credit operation, got %d", walletRepo.applyCallCount())
	}
	if ledgerRepo.insertCallCount() != 1 {
		t.Fatalf("expected only one ledger insert, got %d", ledgerRepo.insertCallCount())
	}
}

func TestCreateTransferConcurrentDifferentTransfersDoNotOverspend(t *testing.T) {
	fromID := uuid.New()
	toID1 := uuid.New()
	toID2 := uuid.New()
	amount := decimal.NewFromInt(10)
	transferRepo := newConcurrentTransferRepo(2)
	txManager := &concurrentTxManager{}
	walletRepo := newConcurrentWalletRepo(map[uuid.UUID]*domain.Wallet{
		fromID: {ID: fromID, Balance: decimal.NewFromInt(15)},
		toID1:  {ID: toID1, Balance: decimal.Zero},
		toID2:  {ID: toID2, Balance: decimal.Zero},
	})
	ledgerRepo := &concurrentLedgerRepo{}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)

	for _, req := range []service.CreateTransferRequest{
		{IdempotencyKey: "debit-race-1", FromWalletID: fromID, ToWalletID: toID1, Amount: amount},
		{IdempotencyKey: "debit-race-2", FromWalletID: fromID, ToWalletID: toID2, Amount: amount},
	} {
		wg.Add(1)
		go func(req service.CreateTransferRequest) {
			defer wg.Done()
			<-start
			_, err := svc.CreateTransfer(context.Background(), req)
			errorsCh <- err
		}(req)
	}

	close(start)
	wg.Wait()
	close(errorsCh)

	successes := 0
	insufficientFunds := 0
	for err := range errorsCh {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, domain.ErrInsufficientFunds):
			insufficientFunds++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if successes != 1 || insufficientFunds != 1 {
		t.Fatalf("expected one success and one insufficient-funds failure, got success=%d insufficient=%d",
			successes, insufficientFunds)
	}
	if walletRepo.applyCallCount() != 1 {
		t.Fatalf("expected only one debit/credit operation, got %d", walletRepo.applyCallCount())
	}
	if ledgerRepo.insertCallCount() != 1 {
		t.Fatalf("expected only one ledger insert, got %d", ledgerRepo.insertCallCount())
	}
}

func TestCreateTransferMarksFailedWhenLedgerInsertFails(t *testing.T) {
	fromID := uuid.New()
	toID := uuid.New()
	ledgerErr := errors.New("ledger insert failed")
	transferRepo := &fakeTransferRepo{}
	txManager := &fakeTxManager{}
	walletRepo := &fakeWalletRepo{
		wallets: map[uuid.UUID]*domain.Wallet{
			fromID: {ID: fromID, Balance: decimal.NewFromInt(100)},
			toID:   {ID: toID, Balance: decimal.NewFromInt(100)},
		},
	}
	ledgerRepo := &fakeLedgerRepo{err: ledgerErr}
	svc := newTestTransferService(txManager, transferRepo, walletRepo, ledgerRepo)

	resp, err := svc.CreateTransfer(context.Background(), service.CreateTransferRequest{
		IdempotencyKey: "key-ledger-fail",
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         decimal.NewFromInt(10),
	})

	if resp != nil {
		t.Fatalf("expected nil response, got %#v", resp)
	}
	if !errors.Is(err, ledgerErr) {
		t.Fatalf("expected ledger error, got %v", err)
	}
	if transferRepo.updateStatusCalls != 1 || transferRepo.updatedStatus != domain.StatusFailed {
		t.Fatalf("expected failed status update, calls=%d status=%s",
			transferRepo.updateStatusCalls, transferRepo.updatedStatus)
	}
	if transferRepo.transitionCalls != 0 {
		t.Fatalf("transition should not happen after ledger failure")
	}
}

func newTestTransferService(
	txManager service.TxManager,
	transferRepo service.TransferRepository,
	walletRepo service.WalletTxRepository,
	ledgerRepo service.LedgerTxRepository,
) *service.TransferService {
	return service.NewTransferService(
		txManager,
		transferRepo,
		walletRepo,
		ledgerRepo,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

type fakeTxManager struct {
	calls int
	err   error
}

func (m *fakeTxManager) WithTx(ctx context.Context, fn func(context.Context) error) error {
	m.calls++
	if m.err != nil {
		return m.err
	}
	return fn(ctx)
}

type fakeTransferRepo struct {
	existing               *domain.Transfer
	existingAfterCreateErr *domain.Transfer
	findErr                error
	createErr              error
	updateStatusErr        error
	transitionErr          error

	created           *domain.Transfer
	updatedStatus     domain.TransferStatus
	failureReason     *string
	transitionFrom    domain.TransferStatus
	transitionTo      domain.TransferStatus
	findCalls         int
	createCalls       int
	updateStatusCalls int
	transitionCalls   int
}

func (r *fakeTransferRepo) FindByIdempotencyKey(context.Context, string) (*domain.Transfer, error) {
	r.findCalls++
	if r.findErr != nil {
		return nil, r.findErr
	}
	if r.findCalls > 1 && r.existingAfterCreateErr != nil {
		return r.existingAfterCreateErr, nil
	}
	return r.existing, nil
}

func (r *fakeTransferRepo) Create(_ context.Context, tr *domain.Transfer) error {
	r.createCalls++
	r.created = tr
	if r.createErr != nil {
		return r.createErr
	}
	tr.CreatedAt = time.Now()
	return nil
}

func (r *fakeTransferRepo) LockByID(_ context.Context, id uuid.UUID) (*domain.Transfer, error) {
	if r.created != nil && r.created.ID == id {
		return r.created, nil
	}
	if r.existing != nil && r.existing.ID == id {
		return r.existing, nil
	}
	if r.existingAfterCreateErr != nil && r.existingAfterCreateErr.ID == id {
		return r.existingAfterCreateErr, nil
	}
	return nil, errors.New("transfer not found")
}

func (r *fakeTransferRepo) UpdateStatus(_ context.Context, _ uuid.UUID, status domain.TransferStatus, reason *string) error {
	r.updateStatusCalls++
	r.updatedStatus = status
	r.failureReason = reason
	return r.updateStatusErr
}

func (r *fakeTransferRepo) Transition(_ context.Context, _ uuid.UUID, from, to domain.TransferStatus) error {
	r.transitionCalls++
	r.transitionFrom = from
	r.transitionTo = to
	if r.created != nil && r.created.Status == from {
		r.created.Status = to
	}
	if r.existing != nil && r.existing.Status == from {
		r.existing.Status = to
	}
	return r.transitionErr
}

type fakeWalletRepo struct {
	wallets       map[uuid.UUID]*domain.Wallet
	lockErr       error
	applyErr      error
	lockOrder     []uuid.UUID
	debitFrom     uuid.UUID
	creditTo      uuid.UUID
	appliedAmount decimal.Decimal
	applyCalls    int
}

func (r *fakeWalletRepo) LockByID(_ context.Context, id uuid.UUID) (*domain.Wallet, error) {
	r.lockOrder = append(r.lockOrder, id)
	if r.lockErr != nil {
		return nil, r.lockErr
	}
	wallet, ok := r.wallets[id]
	if !ok {
		return nil, repository.ErrWalletNotFound
	}
	return wallet, nil
}

func (r *fakeWalletRepo) ApplyDebitCredit(_ context.Context, fromID, toID uuid.UUID, amount decimal.Decimal) error {
	r.applyCalls++
	r.debitFrom = fromID
	r.creditTo = toID
	r.appliedAmount = amount
	return r.applyErr
}

type fakeLedgerRepo struct {
	entries []domain.LedgerEntry
	err     error
}

func (r *fakeLedgerRepo) BulkInsert(_ context.Context, entries []domain.LedgerEntry) error {
	r.entries = append([]domain.LedgerEntry(nil), entries...)
	return r.err
}

type concurrentTxManager struct {
	mu    sync.Mutex
	calls int
}

func (m *concurrentTxManager) WithTx(ctx context.Context, fn func(context.Context) error) error {
	m.mu.Lock()
	m.calls++
	defer m.mu.Unlock()
	return fn(ctx)
}

func (m *concurrentTxManager) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type concurrentTransferRepo struct {
	mu                sync.Mutex
	byKey             map[string]*domain.Transfer
	creates           int
	expectedPrechecks int
	prechecks         int
	prechecksReady    chan struct{}
	readyOnce         sync.Once
}

func newConcurrentTransferRepo(expectedPrechecks int) *concurrentTransferRepo {
	return &concurrentTransferRepo{
		byKey:             make(map[string]*domain.Transfer),
		expectedPrechecks: expectedPrechecks,
		prechecksReady:    make(chan struct{}),
	}
}

func (r *concurrentTransferRepo) FindByIdempotencyKey(_ context.Context, key string) (*domain.Transfer, error) {
	r.mu.Lock()
	r.prechecks++
	initialPrecheck := r.prechecks <= r.expectedPrechecks
	if r.prechecks == r.expectedPrechecks {
		r.readyOnce.Do(func() { close(r.prechecksReady) })
	}
	r.mu.Unlock()

	if initialPrecheck {
		<-r.prechecksReady
		return nil, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byKey[key], nil
}

func (r *concurrentTransferRepo) Create(_ context.Context, tr *domain.Transfer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates++
	if _, exists := r.byKey[tr.IdempotencyKey]; exists {
		return &pgconn.PgError{Code: "23505"}
	}
	tr.CreatedAt = time.Now()
	r.byKey[tr.IdempotencyKey] = tr
	return nil
}

func (r *concurrentTransferRepo) UpdateStatus(context.Context, uuid.UUID, domain.TransferStatus, *string) error {
	return nil
}

func (r *concurrentTransferRepo) LockByID(_ context.Context, id uuid.UUID) (*domain.Transfer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tr := range r.byKey {
		if tr.ID == id {
			return tr, nil
		}
	}
	return nil, errors.New("transfer not found")
}

func (r *concurrentTransferRepo) Transition(_ context.Context, id uuid.UUID, from, to domain.TransferStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tr := range r.byKey {
		if tr.ID == id {
			if tr.Status != from {
				return domain.ErrInvalidTransition
			}
			tr.Status = to
			return nil
		}
	}
	return errors.New("transfer not found")
}

func (r *concurrentTransferRepo) createCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.creates
}

type concurrentWalletRepo struct {
	mu      sync.Mutex
	wallets map[uuid.UUID]*domain.Wallet
	applies int
}

func newConcurrentWalletRepo(wallets map[uuid.UUID]*domain.Wallet) *concurrentWalletRepo {
	return &concurrentWalletRepo{wallets: wallets}
}

func (r *concurrentWalletRepo) LockByID(_ context.Context, id uuid.UUID) (*domain.Wallet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wallet, ok := r.wallets[id]
	if !ok {
		return nil, errors.New("wallet not found")
	}
	return &domain.Wallet{
		ID:      wallet.ID,
		OwnerID: wallet.OwnerID,
		Balance: wallet.Balance,
	}, nil
}

func (r *concurrentWalletRepo) ApplyDebitCredit(_ context.Context, fromID, toID uuid.UUID, amount decimal.Decimal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applies++
	fromWallet, ok := r.wallets[fromID]
	if !ok {
		return errors.New("from wallet not found")
	}
	toWallet, ok := r.wallets[toID]
	if !ok {
		return errors.New("to wallet not found")
	}
	fromWallet.Balance = fromWallet.Balance.Sub(amount)
	toWallet.Balance = toWallet.Balance.Add(amount)
	return nil
}

func (r *concurrentWalletRepo) applyCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applies
}

type concurrentLedgerRepo struct {
	mu      sync.Mutex
	inserts int
}

func (r *concurrentLedgerRepo) BulkInsert(context.Context, []domain.LedgerEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserts++
	return nil
}

func (r *concurrentLedgerRepo) insertCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inserts
}
