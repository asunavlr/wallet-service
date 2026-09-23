//go:build integration

package integration_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/app"
	"github.com/kevinmatos/wallet-service/test/testenv"
)

// A reconciliação lê o saldo e reconstrói o ledger em DOIS statements. O
// enunciado exige que a comparação aconteça "em uma visão consistente dos
// dados": se uma operação commitar entre as duas leituras, a diferença
// aparece sem que nada esteja errado.
//
// Uma divergência falsa não é cosmética — ela dispara a métrica de alarme,
// registra erro no log e responde ao provedor que a carteira está
// inconsistente. Quem investigar não vai achar nada, e na próxima vez que o
// alarme tocar de verdade ninguém vai olhar.
func TestReconciliacaoNaoAcusaDivergenciaFalsaSobConcorrencia(t *testing.T) {
	pool := testenv.Pool(t)
	testenv.Reset(t, pool)
	ws, ts := servicos(pool)
	ctx := context.Background()

	w, err := ws.Open(ctx, app.OpenWallet{PlayerID: uuid.New(), Initial: brl("10000.00")})
	if err != nil {
		t.Fatal(err)
	}

	const apostas = 120
	var wg sync.WaitGroup

	// escritor: aposta sem parar
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < apostas; i++ {
			_, _ = ts.Submit(ctx, aposta(w.ID(), w.PlayerID(), "rec-"+uuid.NewString(), "1.00"))
		}
	}()

	// leitor: reconcilia sem parar, e NENHUMA leitura pode acusar divergência
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < apostas; i++ {
			rec, err := ws.Reconcile(ctx, w.ID())
			if err != nil {
				t.Errorf("reconciliação %d: %v", i, err)
				return
			}
			if !rec.Consistent {
				t.Errorf("divergência FALSA na leitura %d: guardado %s, reconstruído %s, diferença %s",
					i, rec.Stored, rec.Calculated, rec.Difference)
				return
			}
		}
	}()

	wg.Wait()

	// e no fim tudo fecha
	rec, err := ws.Reconcile(ctx, w.ID())
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Consistent || rec.Difference.String() != "0.00" {
		t.Errorf("reconciliação final = %+v", rec)
	}
}
