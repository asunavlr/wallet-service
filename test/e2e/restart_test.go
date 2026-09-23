//go:build e2e

package e2e_test

import (
	"net/http"
	"os/exec"
	"testing"
	"time"
)

// composeDisponivel informa se dá para manipular os containers daqui.
func composeDisponivel(t *testing.T) bool {
	t.Helper()
	if err := exec.Command("docker", "compose", "ps", "-q").Run(); err != nil {
		t.Skipf("docker compose indisponível: %v", err)
		return false
	}
	return true
}

func compose(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose"}, args...)...)
	// Raiz do repositório: o teste roda em test/e2e. O Compose até sobe na
	// árvore procurando o arquivo, mas depender disso é depender de um
	// detalhe da ferramenta.
	cmd.Dir = "../.."
	if saida, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, saida)
	}
}

func esperarSaudavel(t *testing.T, base string, prazo time.Duration) {
	t.Helper()
	limite := time.Now().Add(prazo)
	for time.Now().Before(limite) {
		resp, err := cliente.Get(base + "/health/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s não ficou pronto em %s", base, prazo)
}

// O cenário 8 do enunciado: reiniciar a aplicação e verificar que
// idempotência, pendências e consistência financeira foram preservadas.
//
// Nada disso vive em memória — todo o estado está no PostgreSQL —, então o
// reinício é a prova de que a promessa é real e não apenas cache quente.
func TestEstadoSobreviveAoReinicioCompleto(t *testing.T) {
	if testing.Short() {
		t.Skip("reinicia containers; pulado no modo curto")
	}
	if !composeDisponivel(t) {
		return
	}
	base := instancias()[0]
	tokA := token(t, "provider-a", "provider-a-secret")

	// 1. Uma aposta concluída.
	walletID, player := abrirCarteira(t, base, "100.00")
	sfx := sufixo()
	ext := "rst-" + sfx
	feita := apostar(t, base, tokA, walletID, player, ext, "30.00")
	if feita.Status != http.StatusOK {
		t.Fatalf("aposta = %d: %s", feita.Status, feita.Bruto)
	}
	txOriginal := feita.Corpo["transactionId"]
	saldoOriginal := feita.Corpo["balance"].(map[string]any)["amount"]

	// 2. Uma pendência esperando referência que não existe.
	pend := chamar(t, base, http.MethodPost, "/wagering/transactions", tokA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": "pend-" + sfx,
		"playerId": player, "walletId": walletID,
		"roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "REFUND", "money": dinheiro("30.00"),
		"referenceExternalTransactionId": "alvo-" + sfx,
	}, map[string]string{"Idempotency-Key": "provider-a:pend-" + sfx})
	if pend.Status != http.StatusAccepted {
		t.Fatalf("pendência = %d: %s", pend.Status, pend.Bruto)
	}
	txPendente := pend.Corpo["transactionId"].(string)

	// 3. Reinicia TODAS as instâncias.
	t.Log("reiniciando as três instâncias")
	compose(t, "restart", "app-1", "app-2", "app-3")
	for _, b := range instancias() {
		esperarSaudavel(t, b, 90*time.Second)
	}

	// 4. A idempotência sobreviveu: o replay devolve a mesma transação e o
	//    saldo do processamento original.
	novoTok := token(t, "provider-a", "provider-a-secret")
	replay := apostar(t, base, novoTok, walletID, player, ext, "30.00")
	if replay.Corpo["idempotentReplay"] != true {
		t.Errorf("após reinício, deveria ser replay: %s", replay.Bruto)
	}
	if replay.Corpo["transactionId"] != txOriginal {
		t.Errorf("transação = %v, queria %v", replay.Corpo["transactionId"], txOriginal)
	}
	if got := replay.Corpo["balance"].(map[string]any)["amount"]; got != saldoOriginal {
		t.Errorf("saldo do replay = %v, queria o original %v", got, saldoOriginal)
	}

	// 5. A pendência continua sendo retomada: a referência chega agora e o
	//    worker — de qualquer instância — a resolve.
	alvo := apostar(t, base, novoTok, walletID, player, "alvo-"+sfx, "30.00")
	if alvo.Status != http.StatusOK {
		t.Fatalf("referência = %d: %s", alvo.Status, alvo.Bruto)
	}
	esperar(t, func() bool {
		r := chamar(t, base, http.MethodGet, "/wagering/transactions/"+txPendente, novoTok, nil, nil)
		return r.Corpo["status"] == "PROCESSED"
	}, 60*time.Second, "pendência retomada depois do reinício")

	// 6. E a consistência financeira: 100 - 30 - 30 + 30 = 70.
	w := chamar(t, base, http.MethodGet, "/wallets/"+walletID, novoTok, nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "70.00" {
		t.Errorf("saldo final = %v, queria 70.00", s["amount"])
	}
	rec := chamar(t, base, http.MethodPost, "/wallets/"+walletID+"/reconciliation", novoTok, nil, nil)
	if rec.Corpo["consistent"] != true {
		t.Errorf("reconciliação após reinício = %s", rec.Bruto)
	}
	if d := rec.Corpo["difference"].(map[string]any); d["amount"] != "0.00" {
		t.Errorf("diferença = %v, queria 0.00", d["amount"])
	}
}

// Matar uma instância no meio não perde operação: as outras continuam, e o
// estado no banco é a fonte da verdade.
func TestQuedaDeUmaInstanciaNaoPerdeOperacao(t *testing.T) {
	if testing.Short() {
		t.Skip("derruba um container; pulado no modo curto")
	}
	if !composeDisponivel(t) {
		return
	}
	bases := instancias()
	if len(bases) < 3 {
		t.Skip("precisa das três instâncias")
	}
	tokA := token(t, "provider-a", "provider-a-secret")
	walletID, player := abrirCarteira(t, bases[0], "100.00")
	sfx := sufixo()

	// aposta na instância 1
	r1 := apostar(t, bases[0], tokA, walletID, player, "kill-a-"+sfx, "20.00")
	if r1.Status != http.StatusOK {
		t.Fatalf("primeira = %d: %s", r1.Status, r1.Bruto)
	}

	// mata a instância 1 sem cerimônia
	t.Log("derrubando app-1 com SIGKILL")
	compose(t, "kill", "-s", "SIGKILL", "app-1")
	t.Cleanup(func() {
		compose(t, "start", "app-1")
		esperarSaudavel(t, bases[0], 90*time.Second)
	})

	// as outras continuam operando a mesma carteira
	r2 := apostar(t, bases[1], tokA, walletID, player, "kill-b-"+sfx, "30.00")
	if r2.Status != http.StatusOK {
		t.Fatalf("segunda instância = %d: %s", r2.Status, r2.Bruto)
	}

	// e o replay da primeira aposta, feito numa terceira instância, devolve
	// o resultado gravado antes da queda
	replay := apostar(t, bases[2], tokA, walletID, player, "kill-a-"+sfx, "20.00")
	if replay.Corpo["idempotentReplay"] != true {
		t.Errorf("deveria ser replay: %s", replay.Bruto)
	}
	if replay.Corpo["transactionId"] != r1.Corpo["transactionId"] {
		t.Error("o replay deveria devolver a transação original")
	}

	w := chamar(t, bases[2], http.MethodGet, "/wallets/"+walletID, tokA, nil, nil)
	if s := w.Corpo["balance"].(map[string]any); s["amount"] != "50.00" {
		t.Errorf("saldo = %v, queria 50.00", s["amount"])
	}
	rec := chamar(t, bases[1], http.MethodPost, "/wallets/"+walletID+"/reconciliation", tokA, nil, nil)
	if rec.Corpo["consistent"] != true {
		t.Errorf("reconciliação = %s", rec.Bruto)
	}
}
