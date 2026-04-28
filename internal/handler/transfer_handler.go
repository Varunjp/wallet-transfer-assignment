package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"wallet-service/internal/domain"
	"wallet-service/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type TransferHandler struct {
	service *service.TransferService
}

func NewTransferHandler(s *service.TransferService) *TransferHandler {
	return &TransferHandler{service: s}
}

type createTransferRequest struct {
	IdempotencyKey string          `json:"idempotencyKey"`
	FromWalletID   string          `json:"fromWalletId" binding:"required,uuid"`
	ToWalletID     string          `json:"toWalletId" binding:"required,uuid"`
	Amount         json.RawMessage `json:"amount" binding:"required"`
}

func (h *TransferHandler) CreateTransfer(c *gin.Context) {

	var req createTransferRequest

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "invalid request body",
		})
		return
	}

	idempotencyKey := c.GetHeader("Idempotency-Key")
	if idempotencyKey == "" {
		idempotencyKey = req.IdempotencyKey
	}

	if idempotencyKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "idempotency key required",
		})
		return
	}

	fromID, err := uuid.Parse(req.FromWalletID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid fromWalletId"})
		return
	}

	toID, err := uuid.Parse(req.ToWalletID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid toWalletId"})
		return
	}

	amount, err := parseTransferAmount(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}

	transfer, err := h.service.CreateTransfer(c.Request.Context(), service.CreateTransferRequest{
		IdempotencyKey: idempotencyKey,
		FromWalletID:   fromID,
		ToWalletID:     toID,
		Amount:         amount,
	})

	if err != nil {
		if errors.Is(err, service.ErrInvalidInput) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, domain.ErrInsufficientFunds) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, service.ErrWalletNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if errors.Is(err, service.ErrIdempotencyConflict) || errors.Is(err, service.ErrTransferStillPending) {
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "failed to process transfer",
		})
		return
	}

	c.JSON(http.StatusOK, transfer)
}

func parseTransferAmount(raw json.RawMessage) (decimal.Decimal, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return decimal.Zero, errors.New("amount required")
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		value = strings.TrimSpace(text)
	}

	return decimal.NewFromString(value)
}
