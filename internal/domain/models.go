package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// --- Transfer

type TransferStatus string

const (
	StatusPending   TransferStatus = "PENDING"
	StatusProcessed TransferStatus = "PROCESSED"
	StatusFailed    TransferStatus = "FAILED"
)

var ErrInvalidTransition = errors.New("invalid state transition")

type Transfer struct {
	ID 				uuid.UUID
	IdempotencyKey 	string 
	FromWalletID 	uuid.UUID
	ToWalletID 		uuid.UUID
	Amount 			decimal.Decimal
	Status 			TransferStatus
	FailureReason   string 
	CreatedAt		time.Time
	UpdatedAt 		time.Time
}

// Allowed transition enforced
func (t *Transfer) CanTransitionTo(next TransferStatus) bool {
	allowed := map[TransferStatus][]TransferStatus{
		StatusPending: {StatusProcessed,StatusFailed},
	}
	for _,s := range allowed[t.Status] {
		if s == next {
			return true 
		}
	}
	return false 
}

// --- Wallet 

type Wallet struct {
	ID 		  uuid.UUID
	OwnerID   string 
	Balance   decimal.Decimal
	CreatedAt time.Time
	UpdatedAt  time.Time
}

var ErrInsufficientFunds = errors.New("insufficient funds")

// Check if wallet has enough balance for the given amount
func (w *Wallet) ValidateDebit(amount decimal.Decimal) error {
	if w.Balance.LessThan(amount) {
		return ErrInsufficientFunds
	}
	return nil 
}

// --- LedgerEntry

type EntryType string 

const (
	EntryDebit  EntryType = "DEBIT"
	EntryCredit EntryType = "CREDIT"
)

type LedgerEntry struct {
	ID 		 	uuid.UUID
	WalletID 	uuid.UUID
	TransferID 	uuid.UUID
	Type 		EntryType
	Amount 		decimal.Decimal
	CreatedAt   time.Time
}