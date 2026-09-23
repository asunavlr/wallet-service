package event_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/domain/event"
	"github.com/kevinmatos/wallet-service/internal/domain/money"
)

var quando = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// Tipo e versão são fixados pelo CONSTRUTOR, não pelo chamador: assim um
// publicador não inventa um tipo, e o consumidor pode confiar no contrato.
func TestTipoEVersaoVemDoConstrutor(t *testing.T) {
	id, agg := uuid.New(), uuid.New()
	casos := []struct {
		nome string
		env  func() (event.Envelope, error)
		tipo string
	}{
		{"processado", func() (event.Envelope, error) {
			return event.NewProcessed(id, agg, "c", "x", quando, event.ProcessedData{})
		}, event.TypeProcessed},
		{"rejeitado", func() (event.Envelope, error) {
			return event.NewRejected(id, agg, "c", "x", quando, event.RejectedData{})
		}, event.TypeRejected},
		{"saldo alterado", func() (event.Envelope, error) {
			return event.NewBalanceChanged(id, agg, "c", "x", quando, event.BalanceChangedData{})
		}, event.TypeBalanceChanged},
		{"espera por referência", func() (event.Envelope, error) {
			return event.NewPendingReference(id, agg, "c", "x", quando, event.PendingReferenceData{})
		}, event.TypePendingReference},
	}
	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			e, err := c.env()
			if err != nil {
				t.Fatal(err)
			}
			if e.EventType != c.tipo {
				t.Errorf("tipo = %q, queria %q", e.EventType, c.tipo)
			}
			if e.Version != event.Version {
				t.Errorf("versão = %d, queria %d", e.Version, event.Version)
			}
			if e.EventID != id || e.AggregateID != agg {
				t.Error("identidade do evento não foi preservada")
			}
		})
	}
}

// Timestamps em RFC 3339 UTC, como o contrato exige.
func TestOccurredAtEmUTC(t *testing.T) {
	saoPaulo := time.FixedZone("BRT", -3*3600)
	local := time.Date(2026, 9, 23, 9, 0, 0, 0, saoPaulo)

	e, err := event.NewProcessed(uuid.New(), uuid.New(), "c", "", local, event.ProcessedData{})
	if err != nil {
		t.Fatal(err)
	}
	lido, err := time.Parse(time.RFC3339Nano, e.OccurredAt)
	if err != nil {
		t.Fatalf("occurredAt não é RFC 3339: %q", e.OccurredAt)
	}
	if lido.Location() != time.UTC {
		t.Errorf("occurredAt = %q, queria UTC", e.OccurredAt)
	}
	if !lido.Equal(local) {
		t.Errorf("o instante mudou: %v != %v", lido, local)
	}
}

// Valores monetários viajam como string decimal, nunca como número — é o que
// impede o consumidor de reintroduzir float do outro lado.
func TestValoresSaoStringNoJSON(t *testing.T) {
	e, err := event.NewBalanceChanged(uuid.New(), uuid.New(), "c", "", quando,
		event.BalanceChangedData{
			WalletID: uuid.New(), TransactionID: uuid.New(), Direction: "DEBIT",
			Money:         event.Of(money.MustParse("25.00", "BRL")),
			BalanceBefore: event.Of(money.MustParse("100.00", "BRL")),
			BalanceAfter:  event.Of(money.MustParse("75.00", "BRL")),
			WalletVersion: 2,
		})
	if err != nil {
		t.Fatal(err)
	}
	bruto, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var visto map[string]any
	if err := json.Unmarshal(bruto, &visto); err != nil {
		t.Fatal(err)
	}
	dados := visto["data"].(map[string]any)
	for _, campo := range []string{"money", "balanceBefore", "balanceAfter"} {
		valor := dados[campo].(map[string]any)["amount"]
		if _, ehString := valor.(string); !ehString {
			t.Errorf("%s.amount = %T, queria string", campo, valor)
		}
	}

	// e os campos que o enunciado nomeia estão todos lá
	for _, campo := range []string{"walletId", "transactionId", "direction", "money",
		"balanceBefore", "balanceAfter", "walletVersion"} {
		if _, existe := dados[campo]; !existe {
			t.Errorf("WalletBalanceChanged sem o campo %q", campo)
		}
	}
}

// O envelope carrega o que o enunciado exige.
func TestEnvelopeCompleto(t *testing.T) {
	e, err := event.NewProcessed(uuid.New(), uuid.New(), "corr-1", "caus-1", quando,
		event.ProcessedData{Kind: "BET"})
	if err != nil {
		t.Fatal(err)
	}
	bruto, _ := e.Marshal()
	var visto map[string]any
	_ = json.Unmarshal(bruto, &visto)
	for _, campo := range []string{"eventId", "eventType", "aggregateId",
		"correlationId", "causationId", "occurredAt", "version", "data"} {
		if _, existe := visto[campo]; !existe {
			t.Errorf("envelope sem o campo %q", campo)
		}
	}
}

// causationId é opcional: ausente, não polui o envelope.
func TestCausationOpcional(t *testing.T) {
	e, _ := event.NewProcessed(uuid.New(), uuid.New(), "c", "", quando, event.ProcessedData{})
	bruto, _ := e.Marshal()
	var visto map[string]any
	_ = json.Unmarshal(bruto, &visto)
	if _, existe := visto["causationId"]; existe {
		t.Error("causationId vazio deveria ser omitido")
	}
}
