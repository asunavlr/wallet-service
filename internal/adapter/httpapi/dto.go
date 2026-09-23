package httpapi

import (
	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/contract"
	"github.com/kevinmatos/wallet-service/internal/domain/wallet"
)

type abrirCarteira struct {
	PlayerID       string           `json:"playerId"`
	InitialBalance *contract.Amount `json:"initialBalance"`
}

type carteiraResp struct {
	ID       uuid.UUID       `json:"id"`
	PlayerID uuid.UUID       `json:"playerId"`
	Balance  contract.Amount `json:"balance"`
	Version  int64           `json:"version"`
}

func deCarteira(w *wallet.Wallet) carteiraResp {
	return carteiraResp{
		ID: w.ID(), PlayerID: w.PlayerID(),
		Balance: contract.Of(w.Balance()), Version: w.Version(),
	}
}

type operacaoResp struct {
	TransactionID    uuid.UUID        `json:"transactionId"`
	Status           string           `json:"status"`
	Balance          *contract.Amount `json:"balance,omitempty"`
	FailureCode      string           `json:"failureCode,omitempty"`
	IdempotentReplay bool             `json:"idempotentReplay"`
}

func deResultado(r app.Result) operacaoResp {
	resp := operacaoResp{
		TransactionID: r.TransactionID, Status: string(r.Status),
		FailureCode: string(r.FailureCode), IdempotentReplay: r.Replay,
	}
	if r.Balance != nil {
		a := contract.Of(*r.Balance)
		resp.Balance = &a
	}
	return resp
}

type lancamentoResp struct {
	ID            uuid.UUID       `json:"id"`
	TransactionID uuid.UUID       `json:"transactionId"`
	Direction     string          `json:"direction"`
	Money         contract.Amount `json:"money"`
	BalanceBefore contract.Amount `json:"balanceBefore"`
	BalanceAfter  contract.Amount `json:"balanceAfter"`
	CreatedAt     string          `json:"createdAt"`
}

type paginaLedger struct {
	Items      []lancamentoResp `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

type reconciliacaoResp struct {
	WalletID          uuid.UUID       `json:"walletId"`
	StoredBalance     contract.Amount `json:"storedBalance"`
	CalculatedBalance contract.Amount `json:"calculatedBalance"`
	Difference        contract.Amount `json:"difference"`
	Consistent        bool            `json:"consistent"`
	CheckedEntries    int             `json:"checkedEntries"`
}

type transacaoResp struct {
	TransactionID  uuid.UUID        `json:"transactionId"`
	ProviderID     string           `json:"providerId,omitempty"`
	ExternalID     string           `json:"externalTransactionId,omitempty"`
	WalletID       uuid.UUID        `json:"walletId"`
	Kind           string           `json:"kind"`
	Status         string           `json:"status"`
	Money          contract.Amount  `json:"money"`
	Balance        *contract.Amount `json:"balance,omitempty"`
	FailureCode    string           `json:"failureCode,omitempty"`
	ReferenceExtID string           `json:"referenceExternalTransactionId,omitempty"`
	Attempts       int              `json:"attempts,omitempty"`
	CreatedAt      string           `json:"createdAt"`
	UpdatedAt      string           `json:"updatedAt"`
}
