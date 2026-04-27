package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"wallet-service/internal/domain"
	"wallet-service/internal/handler"
	"wallet-service/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestCreateTransferHandlerValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fromID := uuid.New()
	toID := uuid.New()

	tests := []struct {
		name           string
		body           string
		headerKey      string
		wantStatusCode int
		wantError      string
	}{
		{
			name:           "invalid json body",
			body:           `{"fromWalletId":`,
			wantStatusCode: http.StatusBadRequest,
			wantError:      "invalid request body",
		},
		{
			name: "missing idempotency key",
			body: transferRequestBody(map[string]string{
				"fromWalletId": fromID.String(),
				"toWalletId":   toID.String(),
				"amount":       "10",
			}),
			wantStatusCode: http.StatusBadRequest,
			wantError:      "idempotency key required",
		},
		{
			name: "invalid from wallet id",
			body: transferRequestBody(map[string]string{
				"idempotencyKey": "key-1",
				"fromWalletId":   "not-a-uuid",
				"toWalletId":     toID.String(),
				"amount":         "10",
			}),
			wantStatusCode: http.StatusBadRequest,
			wantError:      "invalid request body",
		},
		{
			name: "invalid amount",
			body: transferRequestBody(map[string]string{
				"idempotencyKey": "key-1",
				"fromWalletId":   fromID.String(),
				"toWalletId":     toID.String(),
				"amount":         "0",
			}),
			wantStatusCode: http.StatusBadRequest,
			wantError:      "invalid amount",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := newTransferTestRouter(newTestTransferService(
				&fakeTxManager{},
				&fakeTransferRepo{},
				&fakeWalletRepo{},
				&fakeLedgerRepo{},
			))

			rec := performTransferRequest(router, tt.body, tt.headerKey)

			if rec.Code != tt.wantStatusCode {
				t.Fatalf("expected status %d, got %d body=%s", tt.wantStatusCode, rec.Code, rec.Body.String())
			}
			assertJSONError(t, rec, tt.wantError)
		})
	}
}

func TestCreateTransferHandlerUsesHeaderIdempotencyKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fromID := uuid.New()
	toID := uuid.New()
	transferRepo := &fakeTransferRepo{}
	walletRepo := &fakeWalletRepo{
		wallets: map[uuid.UUID]*domain.Wallet{
			fromID: {ID: fromID, Balance: decimal.NewFromInt(100)},
			toID:   {ID: toID, Balance: decimal.NewFromInt(100)},
		},
	}
	router := newTransferTestRouter(newTestTransferService(
		&fakeTxManager{},
		transferRepo,
		walletRepo,
		&fakeLedgerRepo{},
	))

	rec := performTransferRequest(router, transferRequestBody(map[string]string{
		"idempotencyKey": "body-key",
		"fromWalletId":   fromID.String(),
		"toWalletId":     toID.String(),
		"amount":         "10",
	}), "header-key")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if transferRepo.created == nil {
		t.Fatal("expected transfer to be created")
	}
	if transferRepo.created.IdempotencyKey != "header-key" {
		t.Fatalf("expected header idempotency key to win, got %q", transferRepo.created.IdempotencyKey)
	}
}

func TestCreateTransferHandlerReturnsBadRequestForInsufficientFunds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fromID := uuid.New()
	toID := uuid.New()
	router := newTransferTestRouter(newTestTransferService(
		&fakeTxManager{},
		&fakeTransferRepo{},
		&fakeWalletRepo{
			wallets: map[uuid.UUID]*domain.Wallet{
				fromID: {ID: fromID, Balance: decimal.NewFromInt(5)},
				toID:   {ID: toID, Balance: decimal.NewFromInt(100)},
			},
		},
		&fakeLedgerRepo{},
	))

	rec := performTransferRequest(router, transferRequestBody(map[string]string{
		"idempotencyKey": "key-insufficient",
		"fromWalletId":   fromID.String(),
		"toWalletId":     toID.String(),
		"amount":         "10",
	}), "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec, domain.ErrInsufficientFunds.Error())
}

func TestCreateTransferHandlerReturnsNotFoundForMissingWallet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fromID := uuid.New()
	toID := uuid.New()
	router := newTransferTestRouter(newTestTransferService(
		&fakeTxManager{},
		&fakeTransferRepo{},
		&fakeWalletRepo{
			wallets: map[uuid.UUID]*domain.Wallet{
				fromID: {ID: fromID, Balance: decimal.NewFromInt(100)},
			},
		},
		&fakeLedgerRepo{},
	))

	rec := performTransferRequest(router, transferRequestBody(map[string]string{
		"idempotencyKey": "key-missing-wallet",
		"fromWalletId":   fromID.String(),
		"toWalletId":     toID.String(),
		"amount":         "10",
	}), "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec, service.ErrWalletNotFound.Error())
}

func TestCreateTransferHandlerReturnsConflictForIdempotencyMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fromID := uuid.New()
	toID := uuid.New()
	router := newTransferTestRouter(newTestTransferService(
		&fakeTxManager{},
		&fakeTransferRepo{
			existing: &domain.Transfer{
				ID:             uuid.New(),
				IdempotencyKey: "repeat-key",
				FromWalletID:   fromID,
				ToWalletID:     toID,
				Amount:         decimal.NewFromInt(10),
				Status:         domain.StatusProcessed,
			},
		},
		&fakeWalletRepo{},
		&fakeLedgerRepo{},
	))

	rec := performTransferRequest(router, transferRequestBody(map[string]string{
		"idempotencyKey": "repeat-key",
		"fromWalletId":   fromID.String(),
		"toWalletId":     toID.String(),
		"amount":         "11",
	}), "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d body=%s", rec.Code, rec.Body.String())
	}
	assertJSONError(t, rec, service.ErrIdempotencyConflict.Error())
}

func newTransferTestRouter(svc *service.TransferService) *gin.Engine {
	router := gin.New()
	handler.RegisterRoutes(router, handler.NewTransferHandler(svc))
	return router
}

func performTransferRequest(router http.Handler, body string, idempotencyKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/transfers", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func transferRequestBody(values map[string]string) string {
	body, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func assertJSONError(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v body=%s", err, rec.Body.String())
	}
	if got["error"] != want {
		t.Fatalf("expected error %q, got %q", want, got["error"])
	}
}
