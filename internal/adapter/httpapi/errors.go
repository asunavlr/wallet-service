package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/internal/contract"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

// Problem é o corpo de erro. Um código estável mais uma mensagem legível: o
// código é para a máquina do provedor decidir, a mensagem é para a pessoa que
// está depurando.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// A tabela abaixo é o contrato de erro da API, e está documentada no README.
// As quatro situações que o enunciado manda distinguir aparecem aqui:
// entrada inválida, conflito, rejeição de negócio e indisponibilidade
// transitória — e cada uma tem código HTTP próprio.
var statusPorErro = []struct {
	err    error
	status int
	code   string
}{
	{contract.ErrDecode, http.StatusBadRequest, "INVALID_REQUEST"},
	{app.ErrInvalidInput, http.StatusBadRequest, "INVALID_REQUEST"},
	{app.ErrForbidden, http.StatusForbidden, "FORBIDDEN"},
	{app.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
	{app.ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
	{app.ErrDuplicateExternalTransaction, http.StatusConflict, "DUPLICATE_EXTERNAL_TRANSACTION"},
	// Conflito de concorrência que sobreviveu aos retries: 503 com Retry-After.
	// Não é 500 — o pedido continua válido, só precisa ser repetido.
	{app.ErrConflict, http.StatusServiceUnavailable, "CONCURRENCY_CONFLICT"},
	{app.ErrUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
}

func escreverErro(w http.ResponseWriter, err error) {
	for _, m := range statusPorErro {
		if errors.Is(err, m.err) {
			if m.status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "1")
			}
			escreverJSON(w, m.status, Problem{Code: m.code, Message: err.Error()})
			return
		}
	}
	// Erro não classificado não vaza detalhe interno para o provedor.
	escreverJSON(w, http.StatusInternalServerError, Problem{
		Code: "INTERNAL_ERROR", Message: "erro interno",
	})
}

// statusDaOperacao traduz o desfecho de uma operação em código HTTP.
//
// Rejeição de negócio é 422: o pedido foi entendido e processado, e a resposta
// é uma recusa registrada e auditável — não é erro de cliente (400) nem falha
// do servidor (500). Espera por referência é 202: aceito, ainda não concluído.
func statusDaOperacao(s wager.Status) int {
	switch s {
	case wager.Processed:
		return http.StatusOK
	case wager.Rejected:
		return http.StatusUnprocessableEntity
	case wager.PendingReference:
		return http.StatusAccepted
	default:
		return http.StatusOK
	}
}

func escreverJSON(w http.ResponseWriter, status int, corpo any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(corpo)
}
