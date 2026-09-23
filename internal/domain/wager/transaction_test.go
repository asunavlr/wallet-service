package wager_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/wager"
)

func entradaExterna(k wager.Kind, valor string) wager.ExternalInput {
	in := wager.ExternalInput{
		ID: uuid.New(), ProviderID: "provider-a", ExternalID: "tx-1",
		IdempotencyKey: "provider-a:tx-1", PayloadHash: "h",
		PlayerID: uuid.New(), WalletID: uuid.New(),
		RoundID: "round-1", GameID: "game-1", Kind: k, Amount: brl(valor),
	}
	if k.RequiresReference() {
		in.ReferenceExtID = "tx-0"
	}
	return in
}

func TestNovaTransacaoComecaEmPending(t *testing.T) {
	tx, err := wager.NewExternal(entradaExterna(wager.Bet, "25.00"), agora)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Status() != wager.Pending {
		t.Errorf("estado inicial = %s, queria PENDING", tx.Status())
	}
	if tx.Origin() != wager.External {
		t.Errorf("origem = %s, queria EXTERNAL", tx.Origin())
	}
}

func TestPoliticaDeValorZeroNaConstrucao(t *testing.T) {
	if _, err := wager.NewExternal(entradaExterna(wager.Loss, "0.00"), agora); err != nil {
		t.Errorf("LOSS com 0.00 deveria ser aceito: %v", err)
	}
	if _, err := wager.NewExternal(entradaExterna(wager.Loss, "5.00"), agora); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("LOSS com valor deveria ser recusado")
	}
	for _, k := range []wager.Kind{wager.Bet, wager.Win, wager.Refund, wager.Rollback} {
		if _, err := wager.NewExternal(entradaExterna(k, "0.00"), agora); !errors.Is(err, wager.ErrInvalidTransaction) {
			t.Errorf("%s com 0.00 deveria ser recusado", k)
		}
	}
}

func TestReferenciaObrigatoriaOpcionalEProibida(t *testing.T) {
	// obrigatória nas reversões
	for _, k := range []wager.Kind{wager.Refund, wager.Rollback} {
		in := entradaExterna(k, "10.00")
		in.ReferenceExtID = ""
		if _, err := wager.NewExternal(in, agora); !errors.Is(err, wager.ErrInvalidTransaction) {
			t.Errorf("%s sem referência deveria ser recusado", k)
		}
	}
	// proibida em BET e LOSS
	for _, k := range []wager.Kind{wager.Bet, wager.Loss} {
		valor := "10.00"
		if k == wager.Loss {
			valor = "0.00"
		}
		in := entradaExterna(k, valor)
		in.ReferenceExtID = "tx-0"
		if _, err := wager.NewExternal(in, agora); !errors.Is(err, wager.ErrInvalidTransaction) {
			t.Errorf("%s com referência deveria ser recusado", k)
		}
	}
	// opcional no WIN
	in := entradaExterna(wager.Win, "10.00")
	in.ReferenceExtID = "tx-0"
	if _, err := wager.NewExternal(in, agora); err != nil {
		t.Errorf("WIN com referência deveria ser aceito: %v", err)
	}
}

func TestOpeningNaoAceitaMetadadosExternos(t *testing.T) {
	in := entradaExterna(wager.Opening, "10.00")
	if _, err := wager.NewExternal(in, agora); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("OPENING por via externa deveria ser recusado")
	}

	tx, err := wager.NewOpening(uuid.New(), uuid.New(), uuid.New(), brl("1000.00"), "c", agora)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Origin() != wager.Internal || tx.Status() != wager.Processed {
		t.Errorf("abertura = %s/%s, queria INTERNAL/PROCESSED", tx.Origin(), tx.Status())
	}
	if tx.ProviderID() != "" || tx.RoundID() != "" || tx.GameID() != "" || tx.IdempotencyKey() != "" {
		t.Error("abertura não pode carregar metadados externos")
	}
	if tx.ResultBalance() == nil || tx.ResultBalance().String() != "1000.00" {
		t.Error("abertura deveria registrar o saldo resultante")
	}
}

// Estado terminal é terminal: nenhuma transição depois dele.
func TestEstadoTerminalNaoTransiciona(t *testing.T) {
	terminais := map[string]func() *wager.Transaction{
		"PROCESSED": func() *wager.Transaction {
			tx, _ := wager.NewExternal(entradaExterna(wager.Bet, "25.00"), agora)
			_ = tx.Process(brl("75.00"), nil, agora)
			return tx
		},
		"REJECTED": func() *wager.Transaction {
			tx, _ := wager.NewExternal(entradaExterna(wager.Bet, "25.00"), agora)
			_ = tx.Reject(wager.InsufficientFunds, nil, agora)
			return tx
		},
		"FAILED": func() *wager.Transaction {
			tx, _ := wager.NewExternal(entradaExterna(wager.Bet, "25.00"), agora)
			_ = tx.Fail(wager.InternalFailure, agora)
			return tx
		},
	}
	for nome, construir := range terminais {
		t.Run(nome, func(t *testing.T) {
			tx := construir()
			if err := tx.Process(brl("1.00"), nil, agora); !errors.Is(err, wager.ErrTerminal) {
				t.Errorf("Process após %s = %v", nome, err)
			}
			if err := tx.Reject(wager.InvalidAmount, nil, agora); !errors.Is(err, wager.ErrTerminal) {
				t.Errorf("Reject após %s = %v", nome, err)
			}
			if err := tx.AwaitReference(agora.Add(time.Minute), agora); !errors.Is(err, wager.ErrTerminal) {
				t.Errorf("AwaitReference após %s = %v", nome, err)
			}
		})
	}
}

// PENDING_REFERENCE não é terminal: é a única espera durável e pode ser
// retomada por qualquer instância.
func TestPendingReferenceRetomavel(t *testing.T) {
	tx, err := wager.NewExternal(entradaExterna(wager.Refund, "25.00"), agora)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.AwaitReference(agora.Add(time.Minute), agora); err != nil {
		t.Fatal(err)
	}
	if tx.Status() != wager.PendingReference || tx.Attempts() != 1 {
		t.Errorf("= %s tentativas=%d", tx.Status(), tx.Attempts())
	}
	// nova tentativa continua permitida
	if err := tx.AwaitReference(agora.Add(2*time.Minute), agora); err != nil {
		t.Errorf("segunda espera: %v", err)
	}
	if tx.Attempts() != 2 {
		t.Errorf("tentativas = %d, queria 2", tx.Attempts())
	}
	// e pode concluir depois
	ref := uuid.New()
	if err := tx.Process(brl("50.00"), &ref, agora); err != nil {
		t.Errorf("conclusão após espera: %v", err)
	}
}

func TestProcessExigeReferenciaResolvidaNasReversoes(t *testing.T) {
	tx, _ := wager.NewExternal(entradaExterna(wager.Refund, "25.00"), agora)
	if err := tx.Process(brl("50.00"), nil, agora); !errors.Is(err, wager.ErrInvalidTransition) {
		t.Error("REFUND sem referência resolvida deveria ser recusado")
	}

	bet, _ := wager.NewExternal(entradaExterna(wager.Bet, "25.00"), agora)
	ref := uuid.New()
	if err := bet.Process(brl("50.00"), &ref, agora); !errors.Is(err, wager.ErrInvalidTransition) {
		t.Error("BET com referência deveria ser recusado")
	}
}

func TestRejeicaoExigeCodigo(t *testing.T) {
	tx, _ := wager.NewExternal(entradaExterna(wager.Bet, "25.00"), agora)
	if err := tx.Reject("", nil, agora); !errors.Is(err, wager.ErrInvalidTransition) {
		t.Error("rejeição sem código deveria ser recusada")
	}
}

// Reidratar é leitura: não reaplica transição nem movimenta nada.
func TestRehydrateValidaConsistencia(t *testing.T) {
	saldo := brl("10.00")
	valido := wager.RehydrateInput{
		ID: uuid.New(), Origin: wager.External, Kind: wager.Bet, Status: wager.Processed,
		WalletID: uuid.New(), PlayerID: uuid.New(), Amount: brl("25.00"),
		ResultBalance: &saldo, CreatedAt: agora, UpdatedAt: agora,
	}
	if _, err := wager.Rehydrate(valido); err != nil {
		t.Fatalf("linha válida recusada: %v", err)
	}

	semSaldo := valido
	semSaldo.ResultBalance = nil
	if _, err := wager.Rehydrate(semSaldo); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("PROCESSED sem saldo deveria ser recusado")
	}

	semCodigo := valido
	semCodigo.Status = wager.Rejected
	semCodigo.ResultBalance = nil
	if _, err := wager.Rehydrate(semCodigo); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("REJECTED sem código deveria ser recusado")
	}

	semAgenda := valido
	semAgenda.Status = wager.PendingReference
	semAgenda.ResultBalance = nil
	if _, err := wager.Rehydrate(semAgenda); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("PENDING_REFERENCE sem agenda deveria ser recusado")
	}

	// PENDING nunca é persistido, então não pode ser reidratado
	pendente := valido
	pendente.Status = wager.Pending
	pendente.ResultBalance = nil
	if _, err := wager.Rehydrate(pendente); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("PENDING não deveria ser um estado reidratável")
	}

	autoRef := valido
	autoRef.ReferenceID = &autoRef.ID
	if _, err := wager.Rehydrate(autoRef); !errors.Is(err, wager.ErrInvalidTransaction) {
		t.Error("transação referenciando a si mesma deveria ser recusada")
	}
}
