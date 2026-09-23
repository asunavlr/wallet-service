package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/contract"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

// corpoMaximo limita o corpo aceito. Sem limite, um corpo gigante vira
// consumo de memória antes de qualquer validação.
const corpoMaximo = 64 << 10

// Handlers reúne os endpoints.
type Handlers struct {
	wallets *app.WalletService
	wagers  *app.WagerService
	queries Queries
}

// Queries são as leituras que não passam por caso de uso.
type Queries interface {
	TransactionByID(ctx context.Context, id uuid.UUID) (*wager.Transaction, error)
	TransactionByExternalID(ctx context.Context, providerID, externalID string) (*wager.Transaction, error)
}

func NewHandlers(w *app.WalletService, g *app.WagerService, q Queries) *Handlers {
	return &Handlers{wallets: w, wagers: g, queries: q}
}

// POST /wallets — restrito ao serviço interno.
func (h *Handlers) AbrirCarteira(w http.ResponseWriter, r *http.Request) {
	var corpo abrirCarteira
	if err := contract.Decode(r.Body, &corpo, corpoMaximo); err != nil {
		escreverErro(w, err)
		return
	}
	player, err := uuid.Parse(corpo.PlayerID)
	if err != nil {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("playerId não é um UUID")))
		return
	}
	if corpo.InitialBalance == nil {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("initialBalance obrigatório")))
		return
	}
	saldo, err := corpo.InitialBalance.Money()
	if err != nil {
		escreverErro(w, errors.Join(contract.ErrDecode, err))
		return
	}

	carteira, err := h.wallets.Open(r.Context(), app.OpenWallet{
		PlayerID: player, Initial: saldo, CorrelationID: CorrelationID(r.Context()),
	})
	if err != nil {
		// Unicidade (jogador, moeda) chega aqui como conflito, e o enunciado
		// pede exatamente "conflito" para a segunda abertura.
		escreverErro(w, err)
		return
	}
	escreverJSON(w, http.StatusCreated, deCarteira(carteira))
}

// GET /wallets/{walletId}
func (h *Handlers) LerCarteira(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "walletId"))
	if err != nil {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("walletId não é um UUID")))
		return
	}
	carteira, err := h.wallets.Get(r.Context(), id)
	if err != nil {
		escreverErro(w, err)
		return
	}
	escreverJSON(w, http.StatusOK, deCarteira(carteira))
}

// GET /wallets/{walletId}/ledger?cursor=...&limit=50
func (h *Handlers) LerLedger(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "walletId"))
	if err != nil {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("walletId não é um UUID")))
		return
	}
	limite := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 200 {
			escreverErro(w, errors.Join(contract.ErrDecode, errors.New("limit fora do intervalo 1..200")))
			return
		}
		limite = n
	}

	linhas, proximo, err := h.wallets.LedgerPage(r.Context(), id, r.URL.Query().Get("cursor"), limite)
	if err != nil {
		escreverErro(w, err)
		return
	}
	pagina := paginaLedger{Items: make([]lancamentoResp, 0, len(linhas)), NextCursor: proximo}
	for _, l := range linhas {
		e := l.Entry
		pagina.Items = append(pagina.Items, lancamentoResp{
			ID: e.ID(), TransactionID: e.TransactionID(), Direction: string(e.Direction()),
			Money: contract.Of(e.Amount()), BalanceBefore: contract.Of(e.BalanceBefore()),
			BalanceAfter: contract.Of(e.BalanceAfter()),
			CreatedAt:    e.CreatedAt().UTC().Format(time.RFC3339Nano),
		})
	}
	escreverJSON(w, http.StatusOK, pagina)
}

// POST /wallets/{walletId}/reconciliation
func (h *Handlers) Reconciliar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "walletId"))
	if err != nil {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("walletId não é um UUID")))
		return
	}
	rec, err := h.wallets.Reconcile(r.Context(), id)
	if err != nil {
		escreverErro(w, err)
		return
	}
	escreverJSON(w, http.StatusOK, reconciliacaoResp{
		WalletID: rec.WalletID, StoredBalance: contract.Of(rec.Stored),
		CalculatedBalance: contract.Of(rec.Calculated), Difference: contract.Of(rec.Difference),
		Consistent: rec.Consistent, CheckedEntries: rec.CheckedEntries,
	})
}

// POST /wagering/transactions
func (h *Handlers) EnviarOperacao(w http.ResponseWriter, r *http.Request) {
	// O header é obrigatório, e o servidor NÃO substitui silenciosamente uma
	// chave recebida por outra calculada — o enunciado proíbe isso.
	chave := r.Header.Get("Idempotency-Key")
	if chave == "" {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("header Idempotency-Key obrigatório")))
		return
	}

	var corpo contract.Operation
	if err := contract.Decode(r.Body, &corpo, corpoMaximo); err != nil {
		escreverErro(w, err)
		return
	}
	// idempotencyKey no corpo é campo do envelope SQS; por HTTP a chave vem
	// do header, e aceitar os dois abriria espaço para divergirem.
	if corpo.IdempotencyKey != "" {
		escreverErro(w, errors.Join(contract.ErrDecode,
			errors.New("idempotencyKey no corpo não é aceito por HTTP; use o header")))
		return
	}
	dados, err := corpo.Validate()
	if err != nil {
		escreverErro(w, err)
		return
	}

	// A identidade autenticada manda sobre o corpo: um provedor não opera em
	// nome de outro, nem por engano nem de propósito.
	if err := exigirProvedor(r.Context(), corpo.ProviderID); err != nil {
		escreverErro(w, err)
		return
	}

	res, err := h.wagers.Submit(r.Context(), app.Submit{
		ProviderID: corpo.ProviderID, ExternalID: corpo.ExternalID, IdempotencyKey: chave,
		PlayerID: dados.PlayerID, WalletID: dados.WalletID,
		RoundID: corpo.RoundID, GameID: corpo.GameID,
		Kind: dados.Kind, Money: dados.Money, ReferenceExtID: corpo.ReferenceExtID,
		CorrelationID: CorrelationID(r.Context()), Source: "http",
	})
	if err != nil {
		escreverErro(w, err)
		return
	}
	escreverJSON(w, statusDaOperacao(res.Status), deResultado(res))
}

// GET /wagering/transactions/{transactionId}
func (h *Handlers) LerOperacao(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "transactionId"))
	if err != nil {
		escreverErro(w, errors.Join(contract.ErrDecode, errors.New("transactionId não é um UUID")))
		return
	}
	tx, err := h.queries.TransactionByID(r.Context(), id)
	if err != nil {
		escreverErro(w, err)
		return
	}
	// O isolamento vale também na consulta: um provedor não lê a operação de
	// outro. Responder 404 em vez de 403 evita confirmar a existência.
	if err := exigirProvedor(r.Context(), tx.ProviderID()); err != nil {
		escreverErro(w, app.ErrNotFound)
		return
	}
	escreverJSON(w, http.StatusOK, deTransacao(tx))
}

// GET /providers/{providerId}/wagering/transactions/{externalTransactionId}
func (h *Handlers) LerOperacaoPorExterno(w http.ResponseWriter, r *http.Request) {
	provedor := chi.URLParam(r, "providerId")
	if err := exigirProvedor(r.Context(), provedor); err != nil {
		escreverErro(w, err)
		return
	}
	tx, err := h.queries.TransactionByExternalID(r.Context(), provedor, chi.URLParam(r, "externalTransactionId"))
	if err != nil {
		escreverErro(w, err)
		return
	}
	escreverJSON(w, http.StatusOK, deTransacao(tx))
}

func deTransacao(t *wager.Transaction) transacaoResp {
	resp := transacaoResp{
		TransactionID: t.ID(), ProviderID: t.ProviderID(), ExternalID: t.ExternalID(),
		WalletID: t.WalletID(), Kind: string(t.Kind()), Status: string(t.Status()),
		Money: contract.Of(t.Amount()), FailureCode: string(t.FailureCode()),
		ReferenceExtID: t.ReferenceExtID(), Attempts: t.Attempts(),
		CreatedAt: t.CreatedAt().UTC().Format(time.RFC3339Nano),
		UpdatedAt: t.UpdatedAt().UTC().Format(time.RFC3339Nano),
	}
	if b := t.ResultBalance(); b != nil {
		a := contract.Of(*b)
		resp.Balance = &a
	}
	return resp
}
