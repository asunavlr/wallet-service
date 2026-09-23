//go:build e2e

package e2e_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// O PostgreSQL cair no meio do tráfego é o cenário que o enunciado lista
// como "indisponibilidade temporária do PostgreSQL". Nada pode gerar
// movimentação duplicada, saldo negativo ou perda de evento confirmado.
//
// O teste derruba o banco com apostas em voo e o traz de volta. O que se
// verifica não é que tudo dá certo — muita coisa vai falhar, e deve —, mas
// que cada resposta é HONESTA: quem respondeu 200 está no banco, quem
// respondeu erro não moveu dinheiro, e o ledger fecha no fim.
func TestPostgresCaindoNoMeioDoTrafego(t *testing.T) {
	if testing.Short() {
		t.Skip("derruba o banco; pulado no modo curto")
	}
	if !composeDisponivel(t) {
		return
	}
	base := instancias()[0]
	tokA := token(t, "provider-a", "provider-a-secret")
	walletID, player := abrirCarteira(t, base, "10000.00")
	sfx := sufixo()

	type envio struct {
		ext    string
		status int
	}
	var mu sync.Mutex
	var envios []envio
	parar := make(chan struct{})
	var wg sync.WaitGroup

	// Emissores CONCORRENTES, e não um laço sequencial: com um só, a queda
	// do banco bloqueia a única requisição em voo e o teste mal encosta na
	// janela de indisponibilidade. Com vários, dezenas de requisições pegam
	// o banco fora do ar — que é o ponto.
	const emissores = 12
	for e := 0; e < emissores; e++ {
		wg.Add(1)
		go func(emissor int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-parar:
					return
				default:
				}
				ext := fmt.Sprintf("chaos-%s-%d-%d", sfx, emissor, i)
				r := chamarSemPular(base, http.MethodPost, "/wagering/transactions", tokA, map[string]any{
					"providerId": "provider-a", "externalTransactionId": ext,
					"playerId": player, "walletId": walletID,
					"roundId": "round-1", "gameId": "fortune-chimp",
					"kind": "BET", "money": dinheiro("1.00"),
				}, map[string]string{"Idempotency-Key": "provider-a:" + ext})
				mu.Lock()
				envios = append(envios, envio{ext: ext, status: r.Status})
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
			}
		}(e)
	}

	time.Sleep(500 * time.Millisecond)
	t.Log("derrubando o PostgreSQL com apostas em voo")
	compose(t, "stop", "-t", "0", "postgres")
	time.Sleep(3 * time.Second)
	t.Log("trazendo o PostgreSQL de volta")
	compose(t, "start", "postgres")
	time.Sleep(6 * time.Second)
	close(parar)
	wg.Wait()

	esperarSaudavel(t, base, 90*time.Second)

	// Cada 200 precisa estar no banco. Cada erro precisa NÃO ter movido nada.
	tokDepois := token(t, "provider-a", "provider-a-secret")
	var ok, falhas, mentiras, fantasmas int
	for _, e := range envios {
		consulta := chamar(t, base, http.MethodGet,
			"/providers/provider-a/wagering/transactions/"+e.ext, tokDepois, nil, nil)
		gravada := consulta.Status == http.StatusOK

		switch {
		case e.status == http.StatusOK:
			ok++
			if !gravada {
				mentiras++
				t.Errorf("%s: respondeu 200 mas não está no banco", e.ext)
			}
		case e.status == 0 || e.status >= 500:
			falhas++
			// Gravada é aceitável: a resposta pode ter se perdido depois do
			// commit, e é para isso que existe a idempotência. O que não
			// pode é ter movido dinheiro DUAS vezes — o ledger prova abaixo.
			if gravada {
				fantasmas++
			}
		}
	}
	t.Logf("%d envios: %d ok, %d falharam (%d delas gravadas antes da resposta se perder)",
		len(envios), ok, falhas, fantasmas)
	if mentiras > 0 {
		t.Fatalf("%d respostas de sucesso sem registro no banco", mentiras)
	}
	if ok == 0 {
		t.Error("nenhuma operação concluiu antes da queda: o teste não exercitou nada")
	}

	// O invariante que fecha tudo.
	rec := chamar(t, base, http.MethodPost, "/wallets/"+walletID+"/reconciliation", interno(t), nil, nil)
	if rec.Corpo["consistent"] != true {
		t.Fatalf("reconciliação após a queda do banco = %s", rec.Bruto)
	}

	// E nenhum débito duplicado: um lançamento por transação gravada.
	vistos := map[string]bool{}
	cursor, total := "", 0
	for pagina := 0; pagina < 50; pagina++ {
		url := "/wallets/" + walletID + "/ledger?limit=200"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		ledger := chamar(t, base, http.MethodGet, url, interno(t), nil, nil)
		itens, _ := ledger.Corpo["items"].([]any)
		for _, it := range itens {
			tx := it.(map[string]any)["transactionId"].(string)
			if vistos[tx] {
				t.Errorf("transação %s tem mais de um lançamento", tx)
			}
			vistos[tx] = true
			total++
		}
		prox, _ := ledger.Corpo["nextCursor"].(string)
		if prox == "" {
			break
		}
		cursor = prox
	}
	t.Logf("ledger com %d lançamentos, todos de transações distintas", total)
}
