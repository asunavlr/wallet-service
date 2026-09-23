package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/adapter/auth"
	"github.com/kevinmatos/wallet-service/internal/adapter/httpapi"
	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

// autFalso troca a validação criptográfica do token por um mapa, para que o
// teste de ROTEAMENTO E AUTORIZAÇÃO rode sem Keycloak. A integração real com
// o IdP é verificada separadamente, em test/integration — aqui o objetivo é
// exercitar as regras de acesso, não a assinatura JWT.
type autFalso struct{ tokens map[string]auth.Claims }

func (a autFalso) Verify(_ context.Context, bruto string) (auth.Claims, error) {
	c, ok := a.tokens[bruto]
	if !ok {
		return auth.Claims{}, auth.ErrUnauthenticated
	}
	return c, nil
}

func (a autFalso) Internal(c auth.Claims) bool {
	return strings.Contains(c.Scope, "wallets:write")
}

type saudeOK struct{}

func (saudeOK) Live(context.Context) error  { return nil }
func (saudeOK) Ready(context.Context) error { return nil }

// consultasFalsas devolve transações fixas, para exercitar o isolamento.
type consultasFalsas struct {
	porID map[uuid.UUID]*wager.Transaction
}

func (c consultasFalsas) TransactionByID(_ context.Context, id uuid.UUID) (*wager.Transaction, error) {
	t, ok := c.porID[id]
	if !ok {
		return nil, wager.ErrInvalidTransaction
	}
	return t, nil
}

func (c consultasFalsas) TransactionByExternalID(_ context.Context, _, _ string) (*wager.Transaction, error) {
	return nil, wager.ErrInvalidTransaction
}

func servidor(t *testing.T) http.Handler {
	t.Helper()
	a := autFalso{tokens: map[string]auth.Claims{
		"token-a":       {Subject: "provider-a", ProviderID: "provider-a"},
		"token-b":       {Subject: "provider-b", ProviderID: "provider-b"},
		"token-interno": {Subject: "interno", Scope: "wallets:write"},
	}}
	h := httpapi.NewHandlers(nil, nil, consultasFalsas{porID: map[uuid.UUID]*wager.Transaction{}})
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return httpapi.Router(h, a, saudeOK{}, log, nil)
}

func req(t *testing.T, srv http.Handler, metodo, caminho, token, corpo string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(metodo, caminho, strings.NewReader(corpo))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if corpo != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

// Sem credencial, nenhuma rota de negócio responde — e nenhuma delas chega a
// tocar em dado.
func TestRotasDeNegocioExigemCredencial(t *testing.T) {
	srv := servidor(t)
	rotas := []struct{ metodo, caminho string }{
		{http.MethodPost, "/wallets"},
		{http.MethodGet, "/wallets/" + uuid.NewString()},
		{http.MethodGet, "/wallets/" + uuid.NewString() + "/ledger"},
		{http.MethodPost, "/wallets/" + uuid.NewString() + "/reconciliation"},
		{http.MethodPost, "/wagering/transactions"},
		{http.MethodGet, "/wagering/transactions/" + uuid.NewString()},
		{http.MethodGet, "/providers/provider-a/wagering/transactions/tx-1"},
	}
	for _, rota := range rotas {
		t.Run(rota.metodo+" "+rota.caminho, func(t *testing.T) {
			// sem header
			if w := req(t, srv, rota.metodo, rota.caminho, "", "", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("sem token = %d, queria 401", w.Code)
			}
			// token desconhecido
			if w := req(t, srv, rota.metodo, rota.caminho, "token-invalido", "", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("token inválido = %d, queria 401", w.Code)
			}
		})
	}
}

func TestHeaderAuthorizationMalformado(t *testing.T) {
	srv := servidor(t)
	for _, header := range []string{"token-a", "Basic dXNlcjpwYXNz", "Bearer", "Bearer "} {
		r := httptest.NewRequest(http.MethodGet, "/wallets/"+uuid.NewString(), nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("header %q = %d, queria 401", header, w.Code)
		}
	}
}

// Abrir carteira é do serviço interno. Provedor autenticado não passa.
func TestAbrirCarteiraERestritaAoServicoInterno(t *testing.T) {
	srv := servidor(t)
	corpo := `{"playerId":"` + uuid.NewString() + `","initialBalance":{"amount":"100.00","currency":"BRL"}}`

	w := req(t, srv, http.MethodPost, "/wallets", "token-a", corpo, nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("provedor abrindo carteira = %d, queria 403", w.Code)
	}
	var p httpapi.Problem
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Code != "FORBIDDEN" {
		t.Errorf("código = %q, queria FORBIDDEN", p.Code)
	}
}

// O providerId do corpo é conferido contra o token: um provedor não opera em
// nome de outro.
func TestProvedorNaoOperaPorOutro(t *testing.T) {
	srv := servidor(t)
	corpo := `{"providerId":"provider-a","externalTransactionId":"tx-1",` +
		`"playerId":"` + uuid.NewString() + `","walletId":"` + uuid.NewString() + `",` +
		`"roundId":"r1","gameId":"g1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`

	w := req(t, srv, http.MethodPost, "/wagering/transactions", "token-b", corpo,
		map[string]string{"Idempotency-Key": "provider-a:tx-1"})
	if w.Code != http.StatusForbidden {
		t.Errorf("provider-b enviando como provider-a = %d, queria 403", w.Code)
	}
}

// A consulta por provedor também isola: token de b não lê rota de a.
func TestConsultaPorProvedorIsola(t *testing.T) {
	srv := servidor(t)
	w := req(t, srv, http.MethodGet, "/providers/provider-a/wagering/transactions/tx-1", "token-b", "", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("= %d, queria 403", w.Code)
	}
}

// O header Idempotency-Key é obrigatório, e o servidor não inventa um no lugar.
func TestIdempotencyKeyObrigatoria(t *testing.T) {
	srv := servidor(t)
	corpo := `{"providerId":"provider-a","externalTransactionId":"tx-1",` +
		`"playerId":"` + uuid.NewString() + `","walletId":"` + uuid.NewString() + `",` +
		`"roundId":"r1","gameId":"g1","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}`

	w := req(t, srv, http.MethodPost, "/wagering/transactions", "token-a", corpo, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("sem Idempotency-Key = %d, queria 400", w.Code)
	}
}

// Health checks são públicos: um probe não carrega token.
func TestHealthChecksSaoPublicos(t *testing.T) {
	srv := servidor(t)
	for _, caminho := range []string{"/health/live", "/health/ready"} {
		if w := req(t, srv, http.MethodGet, caminho, "", "", nil); w.Code != http.StatusOK {
			t.Errorf("%s = %d, queria 200", caminho, w.Code)
		}
	}
}

// Corpo malformado é recusado antes de qualquer efeito, e o decoder estrito
// pega erro de digitação em nome de campo.
func TestCorpoInvalido(t *testing.T) {
	srv := servidor(t)
	headers := map[string]string{"Idempotency-Key": "provider-a:tx-1"}
	casos := map[string]string{
		"json quebrado":      `{`,
		"campo desconhecido": `{"providerId":"provider-a","inexistente":1}`,
		"lixo após o objeto": `{"providerId":"provider-a"} lixo`,
		"money como número":  `{"providerId":"provider-a","externalTransactionId":"t","playerId":"` + uuid.NewString() + `","walletId":"` + uuid.NewString() + `","roundId":"r","gameId":"g","kind":"BET","money":{"amount":25.00,"currency":"BRL"}}`,
		"OPENING pela borda": `{"providerId":"provider-a","externalTransactionId":"t","playerId":"` + uuid.NewString() + `","walletId":"` + uuid.NewString() + `","roundId":"r","gameId":"g","kind":"OPENING","money":{"amount":"25.00","currency":"BRL"}}`,
		"escala excedente":   `{"providerId":"provider-a","externalTransactionId":"t","playerId":"` + uuid.NewString() + `","walletId":"` + uuid.NewString() + `","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"25.001","currency":"BRL"}}`,
		"valor negativo":     `{"providerId":"provider-a","externalTransactionId":"t","playerId":"` + uuid.NewString() + `","walletId":"` + uuid.NewString() + `","roundId":"r","gameId":"g","kind":"BET","money":{"amount":"-25.00","currency":"BRL"}}`,
	}
	for nome, corpo := range casos {
		t.Run(nome, func(t *testing.T) {
			w := req(t, srv, http.MethodPost, "/wagering/transactions", "token-a", corpo, headers)
			if w.Code != http.StatusBadRequest {
				t.Errorf("= %d, queria 400 (corpo: %s)", w.Code, w.Body.String())
			}
		})
	}
}

// Um provedor não navega em carteira: nem saldo, nem ledger, nem
// reconciliação. O ledger de uma carteira contém as operações de TODOS os
// provedores que a movimentaram, com valor e identificador — deixá-lo aberto
// mostraria a um provedor quanto o jogador apostou no concorrente.
func TestProvedorNaoLeCarteiraNemLedger(t *testing.T) {
	srv := servidor(t)
	w := uuid.NewString()
	rotas := []struct{ metodo, caminho string }{
		{http.MethodGet, "/wallets/" + w},
		{http.MethodGet, "/wallets/" + w + "/ledger"},
		{http.MethodPost, "/wallets/" + w + "/reconciliation"},
	}
	for _, rota := range rotas {
		t.Run(rota.caminho, func(t *testing.T) {
			r := req(t, srv, rota.metodo, rota.caminho, "token-a", "", nil)
			if r.Code != http.StatusForbidden {
				t.Errorf("provedor em %s = %d, queria 403", rota.caminho, r.Code)
			}
		})
	}
}
