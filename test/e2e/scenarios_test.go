//go:build e2e

package e2e_test

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// Health checks respondem sem token em todas as instâncias.
func TestTresInstanciasNoAr(t *testing.T) {
	for _, base := range instancias() {
		r := chamar(t, base, http.MethodGet, "/health/ready", "", nil, nil)
		if r.Status != http.StatusOK {
			t.Errorf("%s /health/ready = %d: %s", base, r.Status, r.Bruto)
		}
	}
}

// Sem token, nenhuma rota de negócio responde — e nenhuma toca em dado.
func TestAcessoNaoAutenticadoNaoTemEfeito(t *testing.T) {
	base := instancias()[0]
	r := chamar(t, base, http.MethodPost, "/wallets", "", map[string]any{
		"playerId": uuidNovo(), "initialBalance": dinheiro("100.00"),
	}, nil)
	if r.Status != http.StatusUnauthorized {
		t.Errorf("abrir carteira sem token = %d, queria 401", r.Status)
	}
}

// Provedor de jogos não abre carteira: isso é do serviço interno.
func TestProvedorNaoAbreCarteira(t *testing.T) {
	base := instancias()[0]
	tokA := token(t, "provider-a", "provider-a-secret")
	r := chamar(t, base, http.MethodPost, "/wallets", tokA, map[string]any{
		"playerId": uuidNovo(), "initialBalance": dinheiro("100.00"),
	}, nil)
	if r.Status != http.StatusForbidden {
		t.Errorf("= %d, queria 403: %s", r.Status, r.Bruto)
	}
}

// abrirCarteira usa o cliente interno, como o sistema faria.
func abrirCarteira(t *testing.T, base, saldo string) (id, player string) {
	t.Helper()
	interno := token(t, "internal-wallet-service", "internal-secret")
	player = uuidNovo()
	r := chamar(t, base, http.MethodPost, "/wallets", interno, map[string]any{
		"playerId": player, "initialBalance": dinheiro(saldo),
	}, nil)
	if r.Status != http.StatusCreated {
		t.Fatalf("abrindo carteira = %d: %s", r.Status, r.Bruto)
	}
	return r.Corpo["id"].(string), player
}

func apostar(t *testing.T, base, tok, walletID, player, extID, valor string) resposta {
	t.Helper()
	return chamar(t, base, http.MethodPost, "/wagering/transactions", tok, map[string]any{
		"providerId": "provider-a", "externalTransactionId": extID,
		"playerId": player, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "BET", "money": dinheiro(valor),
	}, map[string]string{"Idempotency-Key": "provider-a:" + extID})
}

// O cenário obrigatório, agora ponta a ponta e ENTRE INSTÂNCIAS DIFERENTES:
// 100.00 recebendo duas apostas simultâneas de 80.00, cada uma numa
// instância distinta do serviço.
func TestDuasApostasEmInstanciasDiferentes(t *testing.T) {
	bases := instancias()
	if len(bases) < 2 {
		t.Skip("precisa de ao menos duas instâncias")
	}
	walletID, player := abrirCarteira(t, bases[0], "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	var wg sync.WaitGroup
	res := make([]resposta, 2)
	partida := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-partida
			res[i] = apostar(t, bases[i%len(bases)], tokA, walletID, player,
				"tx-"+sfx+"-"+string(rune('a'+i)), "80.00")
		}(i)
	}
	close(partida)
	wg.Wait()

	var ok, recusadas int
	for _, r := range res {
		switch r.Status {
		case http.StatusOK:
			ok++
		case http.StatusUnprocessableEntity:
			recusadas++
			if r.Corpo["failureCode"] != "INSUFFICIENT_FUNDS" {
				t.Errorf("código = %v, queria INSUFFICIENT_FUNDS", r.Corpo["failureCode"])
			}
		default:
			t.Errorf("status inesperado %d: %s", r.Status, r.Bruto)
		}
	}
	if ok != 1 || recusadas != 1 {
		t.Fatalf("= %d aceitas e %d recusadas; queria 1 e 1", ok, recusadas)
	}

	// Saldo final 20.00, lido de uma TERCEIRA instância.
	leitura := chamar(t, bases[len(bases)-1], http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
	saldo := leitura.Corpo["balance"].(map[string]any)
	if saldo["amount"] != "20.00" {
		t.Errorf("saldo = %v, queria 20.00", saldo["amount"])
	}

	// Um único débito, e a reconciliação fecha.
	rec := chamar(t, bases[0], http.MethodPost, "/wallets/"+walletID+"/reconciliation", tokA, nil, nil)
	if rec.Corpo["consistent"] != true {
		t.Errorf("reconciliação = %s", rec.Bruto)
	}
	if d := rec.Corpo["difference"].(map[string]any); d["amount"] != "0.00" {
		t.Errorf("diferença = %v, queria 0.00", d["amount"])
	}
}

// O replay devolve o saldo do processamento original, não o saldo atual, e
// funciona de qualquer instância.
func TestReplayEntreInstancias(t *testing.T) {
	bases := instancias()
	walletID, player := abrirCarteira(t, bases[0], "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")
	extID := "tx-replay-" + sfx

	primeira := apostar(t, bases[0], tokA, walletID, player, extID, "10.00")
	if primeira.Status != http.StatusOK {
		t.Fatalf("primeira = %d: %s", primeira.Status, primeira.Bruto)
	}
	if primeira.Corpo["idempotentReplay"] != false {
		t.Error("a primeira não deveria ser replay")
	}
	saldoOriginal := primeira.Corpo["balance"].(map[string]any)["amount"]

	// A carteira se move num outro nó.
	if r := apostar(t, bases[1%len(bases)], tokA, walletID, player, extID+"-outra", "20.00"); r.Status != http.StatusOK {
		t.Fatalf("segunda = %d: %s", r.Status, r.Bruto)
	}

	// O replay, numa terceira instância, ainda devolve o saldo original.
	replay := apostar(t, bases[len(bases)-1], tokA, walletID, player, extID, "10.00")
	if replay.Corpo["idempotentReplay"] != true {
		t.Errorf("deveria ser replay: %s", replay.Bruto)
	}
	if got := replay.Corpo["balance"].(map[string]any)["amount"]; got != saldoOriginal {
		t.Errorf("replay devolveu %v, queria o saldo original %v", got, saldoOriginal)
	}
	if replay.Corpo["transactionId"] != primeira.Corpo["transactionId"] {
		t.Error("o replay deveria devolver a mesma transação")
	}
}

// Isolamento entre provedores, inclusive no replay: provider-b não alcança
// nada de provider-a.
func TestIsolamentoEntreProvedores(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")
	tokB := token(t, "provider-b", "provider-b-secret")
	extID := "tx-iso-" + sfx

	feita := apostar(t, base, tokA, walletID, player, extID, "25.00")
	if feita.Status != http.StatusOK {
		t.Fatalf("aposta de a = %d: %s", feita.Status, feita.Bruto)
	}
	txID := feita.Corpo["transactionId"].(string)

	// b tentando enviar como a
	comoA := apostar(t, base, tokB, walletID, player, extID+"-b", "25.00")
	if comoA.Status != http.StatusForbidden {
		t.Errorf("b enviando como a = %d, queria 403", comoA.Status)
	}

	// b lendo a transação de a
	leitura := chamar(t, base, http.MethodGet, "/wagering/transactions/"+txID, tokB, nil, nil)
	if leitura.Status != http.StatusNotFound {
		t.Errorf("b lendo transação de a = %d, queria 404", leitura.Status)
	}

	// b lendo pela rota de provedor de a
	porProvedor := chamar(t, base, http.MethodGet,
		"/providers/provider-a/wagering/transactions/"+extID, tokB, nil, nil)
	if porProvedor.Status != http.StatusForbidden {
		t.Errorf("b na rota de a = %d, queria 403", porProvedor.Status)
	}

	// e a própria a lê normalmente
	comA := chamar(t, base, http.MethodGet, "/wagering/transactions/"+txID, tokA, nil, nil)
	if comA.Status != http.StatusOK {
		t.Errorf("a lendo a própria transação = %d", comA.Status)
	}
}

// O evento chega à fila de saída depois do commit — é o outbox funcionando
// ponta a ponta.
func TestEventosSaemDepoisDoCommit(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	r := apostar(t, base, tokA, walletID, player, "tx-ev-"+sfx, "15.00")
	if r.Status != http.StatusOK {
		t.Fatalf("= %d: %s", r.Status, r.Bruto)
	}

	// O ledger precisa refletir a operação, e a reconciliação fechar.
	esperar(t, func() bool {
		rec := chamar(t, base, http.MethodPost, "/wallets/"+walletID+"/reconciliation", tokA, nil, nil)
		return rec.Corpo["consistent"] == true
	}, 10*time.Second, "reconciliação consistente")

	ledger := chamar(t, base, http.MethodGet, "/wallets/"+walletID+"/ledger?limit=10", tokA, nil, nil)
	itens, _ := ledger.Corpo["items"].([]any)
	if len(itens) != 2 { // abertura + aposta
		t.Errorf("lançamentos = %d, queria 2: %s", len(itens), ledger.Bruto)
	}
}
