package app

import (
	"time"

	"github.com/google/uuid"
)

// SystemClock usa o relógio do processo.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }

// UUIDGenerator gera UUIDv7: ordenável por tempo, o que evita fragmentação de
// página num índice primário de tabela append-only como o ledger.
type UUIDGenerator struct{}

func (UUIDGenerator) New() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New() // v4 como reserva; a unicidade é o que importa
	}
	return id
}

// NoMetrics descarta métricas. Serve a testes e a componentes que ainda não
// foram instrumentados.
type NoMetrics struct{}

func (NoMetrics) TransactionResult(_, _, _ string)     {}
func (NoMetrics) Duplicate(string)                     {}
func (NoMetrics) Retry(string)                         {}
func (NoMetrics) DeadLetter(string)                    {}
func (NoMetrics) ConcurrencyConflict()                 {}
func (NoMetrics) ProcessingTime(string, time.Duration) {}
func (NoMetrics) OutboxLag(time.Duration)              {}
func (NoMetrics) ReconciliationDivergence()            {}
