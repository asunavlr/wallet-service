//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"
)

// Reversão que chega antes da referência, ponta a ponta: o REFUND fica em
// 202, a BET chega depois e o worker resolve sozinho.
func TestReversaoAntesDaReferenciaPontaAPonta(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	// o REFUND chega primeiro
	refund := chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": "rf-" + sfx,
		"playerId": player, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": dinheiro("30.00"),
		"referenceExternalTransactionId": "bt-" + sfx,
	}, map[string]string{"Idempotency-Key": "provider-a:rf-" + sfx})

	if refund.Status != http.StatusAccepted {
		t.Fatalf("REFUND sem referência = %d, queria 202: %s", refund.Status, refund.Bruto)
	}
	if refund.Corpo["status"] != "PENDING_REFERENCE" {
		t.Errorf("estado = %v, queria PENDING_REFERENCE", refund.Corpo["status"])
	}

	// agora a aposta
	bet := apostar(t, base, tokA, walletID, player, "bt-"+sfx, "30.00")
	if bet.Status != http.StatusOK {
		t.Fatalf("BET = %d: %s", bet.Status, bet.Bruto)
	}

	// o worker resolve a pendência sozinho, sem intervenção
	txID := refund.Corpo["transactionId"].(string)
	esperar(t, func() bool {
		r := chamar(t, base, http.MethodGet, "/wagering/transactions/"+txID, tokA, nil, nil)
		return r.Corpo["status"] == "PROCESSED"
	}, 30*1000*1000*1000, "REFUND resolvido pelo worker")

	// saldo de volta aos 100.00 e reconciliação fechando
	w := chamar(t, base, http.MethodGet, "/wallets/"+walletID, interno(t), nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "100.00" {
		t.Errorf("saldo = %v, queria 100.00", s["amount"])
	}
	rec := chamar(t, base, http.MethodPost, "/wallets/"+walletID+"/reconciliation", interno(t), nil, nil)
	if rec.Corpo["consistent"] != true {
		t.Errorf("reconciliação = %s", rec.Bruto)
	}
}

// LOSS: processa, não move saldo e não versiona.
func TestLossPontaAPonta(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	antes := chamar(t, base, http.MethodGet, "/wallets/"+walletID, interno(t), nil, nil)
	versaoAntes := antes.Corpo["version"]

	r := chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": "ls-" + sfx,
		"playerId": player, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "LOSS", "money": dinheiro("0.00"),
	}, map[string]string{"Idempotency-Key": "provider-a:ls-" + sfx})

	if r.Status != http.StatusOK || r.Corpo["status"] != "PROCESSED" {
		t.Fatalf("LOSS = %d/%v: %s", r.Status, r.Corpo["status"], r.Bruto)
	}

	depois := chamar(t, base, http.MethodGet, "/wallets/"+walletID, interno(t), nil, nil)
	if depois.Corpo["version"] != versaoAntes {
		t.Errorf("versão mudou de %v para %v; LOSS não versiona", versaoAntes, depois.Corpo["version"])
	}
	if s := depois.Corpo["balance"].(map[string]any); s["amount"] != "100.00" {
		t.Errorf("saldo = %v, queria 100.00 intacto", s["amount"])
	}
}

// LOSS com valor diferente de zero é recusado na borda.
func TestLossComValorERecusado(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	r := chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": "lsx-" + sfx,
		"playerId": player, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "LOSS", "money": dinheiro("5.00"),
	}, map[string]string{"Idempotency-Key": "provider-a:lsx-" + sfx})

	if r.Status != http.StatusBadRequest {
		t.Errorf("= %d, queria 400: %s", r.Status, r.Bruto)
	}
}

// Conflito de idempotência e operação duplicada, pela API.
func TestConflitosPelaAPI(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "500.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")
	ext := "cf-" + sfx
	chave := "provider-a:" + ext

	corpo := map[string]any{
		"providerId": "provider-a", "externalTransactionId": ext,
		"playerId": player, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "BET", "money": dinheiro("25.00"),
	}
	if r := chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, corpo,
		map[string]string{"Idempotency-Key": chave}); r.Status != http.StatusOK {
		t.Fatalf("primeira = %d: %s", r.Status, r.Bruto)
	}

	// mesma chave, valor diferente
	divergente := map[string]any{}
	for k, v := range corpo {
		divergente[k] = v
	}
	divergente["money"] = dinheiro("30.00")
	r := chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, divergente,
		map[string]string{"Idempotency-Key": chave})
	if r.Status != http.StatusConflict || r.Corpo["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Errorf("chave reutilizada = %d/%v, queria 409/IDEMPOTENCY_CONFLICT", r.Status, r.Corpo["code"])
	}

	// mesma operação, outra chave
	r = chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, corpo,
		map[string]string{"Idempotency-Key": "provider-a:outra-chave-" + sfx})
	if r.Status != http.StatusConflict || r.Corpo["code"] != "DUPLICATE_EXTERNAL_TRANSACTION" {
		t.Errorf("outra chave = %d/%v, queria 409/DUPLICATE_EXTERNAL_TRANSACTION", r.Status, r.Corpo["code"])
	}

	// nada disso moveu o saldo além da primeira aposta
	w := chamar(t, base, http.MethodGet, "/wallets/"+walletID, interno(t), nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "475.00" {
		t.Errorf("saldo = %v, queria 475.00", s["amount"])
	}
}

// Segunda carteira para o mesmo jogador e moeda é conflito.
func TestSegundaCarteiraEConflito(t *testing.T) {
	base := instancias()[0]
	interno := token(t, "internal-wallet-service", "internal-secret")
	player := uuidNovo()

	primeira := chamar(t, base, http.MethodPost, "/wallets", interno, map[string]any{
		"playerId": player, "initialBalance": dinheiro("10.00"),
	}, nil)
	if primeira.Status != http.StatusCreated {
		t.Fatalf("primeira = %d: %s", primeira.Status, primeira.Bruto)
	}

	segunda := chamar(t, base, http.MethodPost, "/wallets", interno, map[string]any{
		"playerId": player, "initialBalance": dinheiro("20.00"),
	}, nil)
	if segunda.Status != http.StatusServiceUnavailable && segunda.Status != http.StatusConflict {
		t.Errorf("segunda carteira = %d, queria conflito: %s", segunda.Status, segunda.Bruto)
	}
}

// Paginação do ledger com cursor opaco.
func TestPaginacaoDoLedger(t *testing.T) {
	base := instancias()[0]
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	tokA := token(t, "provider-a", "provider-a-secret")

	for i := 0; i < 5; i++ {
		if r := apostar(t, base, tokA, walletID, player,
			"pg-"+sfx+"-"+string(rune('a'+i)), "1.00"); r.Status != http.StatusOK {
			t.Fatalf("aposta %d = %d: %s", i, r.Status, r.Bruto)
		}
	}

	primeira := chamar(t, base, http.MethodGet, "/wallets/"+walletID+"/ledger?limit=2", interno(t), nil, nil)
	itens, _ := primeira.Corpo["items"].([]any)
	if len(itens) != 2 {
		t.Fatalf("página = %d itens, queria 2: %s", len(itens), primeira.Bruto)
	}
	cursor, ok := primeira.Corpo["nextCursor"].(string)
	if !ok || cursor == "" {
		t.Fatal("deveria haver próximo cursor")
	}

	segunda := chamar(t, base, http.MethodGet,
		"/wallets/"+walletID+"/ledger?limit=2&cursor="+cursor, interno(t), nil, nil)
	itens2, _ := segunda.Corpo["items"].([]any)
	if len(itens2) != 2 {
		t.Errorf("segunda página = %d itens: %s", len(itens2), segunda.Bruto)
	}
	// páginas não se sobrepõem
	if itens[0].(map[string]any)["id"] == itens2[0].(map[string]any)["id"] {
		t.Error("a segunda página repetiu a primeira")
	}

	// cursor inválido é 400
	ruim := chamar(t, base, http.MethodGet, "/wallets/"+walletID+"/ledger?cursor=!!!", interno(t), nil, nil)
	if ruim.Status != http.StatusBadRequest {
		t.Errorf("cursor inválido = %d, queria 400", ruim.Status)
	}
}
