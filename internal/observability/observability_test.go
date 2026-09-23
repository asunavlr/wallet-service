package observability_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kevinmatos/wallet-service/internal/observability"
)

// As oito métricas que o enunciado manda expor precisam existir com o nome
// certo: um dashboard aponta para o nome, e renomear em silêncio apaga o
// painel de quem está de plantão.
func TestMetricasExigidasExistem(t *testing.T) {
	m := observability.NewMetrics()

	// exercita cada uma para que apareçam no /metrics
	m.TransactionResult("BET", "PROCESSED", "http")
	m.Duplicate("sqs")
	m.Retry("outbox")
	m.DeadLetter("sqs")
	m.ConcurrencyConflict()
	m.ProcessingTime("http", 12*time.Millisecond)
	m.OutboxLag(3 * time.Second)
	m.ReconciliationDivergence()

	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", w.Code)
	}
	corpo := w.Body.String()

	exigidas := []string{
		"wagering_transactions_total",
		"wagering_duplicates_total",
		"wagering_retries_total",
		"wagering_dead_letter_total",
		"wallet_lock_conflicts_total",
		"wagering_processing_seconds",
		"outbox_pending_age_seconds",
		"reconciliation_divergences_total",
	}
	for _, nome := range exigidas {
		if !strings.Contains(corpo, nome) {
			t.Errorf("métrica %q não aparece em /metrics", nome)
		}
	}

	// os rótulos que permitem separar origem e tipo
	for _, rotulo := range []string{`kind="BET"`, `status="PROCESSED"`, `source="http"`} {
		if !strings.Contains(corpo, rotulo) {
			t.Errorf("rótulo %s não aparece", rotulo)
		}
	}
}

// Registro PRÓPRIO, e não o global: coletor global vaza entre testes e
// impede duas instâncias no mesmo processo.
func TestRegistroEProprio(t *testing.T) {
	a := observability.NewMetrics()
	b := observability.NewMetrics()

	a.ConcurrencyConflict()
	a.ConcurrencyConflict()

	corpo := func(m *observability.Metrics) string {
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		return w.Body.String()
	}
	if !strings.Contains(corpo(a), "wallet_lock_conflicts_total 2") {
		t.Error("o primeiro registro deveria contar 2")
	}
	if !strings.Contains(corpo(b), "wallet_lock_conflicts_total 0") {
		t.Error("o segundo registro deveria estar zerado: registros não se misturam")
	}
}

func TestNivelDoLog(t *testing.T) {
	for _, nivel := range []string{"debug", "info", "warn", "error", "DEBUG", "desconhecido", ""} {
		if l := observability.NewLogger(nivel); l == nil {
			t.Errorf("NewLogger(%q) devolveu nil", nivel)
		}
	}
	// nível desconhecido cai em info em vez de derrubar o processo
	if !observability.NewLogger("xyz").Enabled(nil, 0) {
		t.Error("nível desconhecido deveria cair em info")
	}
}
