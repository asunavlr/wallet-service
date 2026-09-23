// Package observability reúne log estruturado e métricas.
package observability

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
)

// NewLogger monta o logger JSON.
//
// JSON porque o enunciado pede, e porque log estruturado é o que permite
// filtrar por walletId ou correlationId num incidente. Credencial e payload
// financeiro completo nunca são registrados.
func NewLogger(nivel string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(nivel) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

// Metrics implementa app.Metrics sobre o Prometheus.
//
// Cada métrica corresponde a um item que o enunciado manda expor: resultados
// por status, duplicatas, retries, DLQ, conflitos de concorrência, atraso da
// outbox, latência e divergências de reconciliação.
type Metrics struct {
	registro     *prometheus.Registry
	resultados   *prometheus.CounterVec
	duplicatas   *prometheus.CounterVec
	retries      *prometheus.CounterVec
	dlq          *prometheus.CounterVec
	conflitos    prometheus.Counter
	latencia     *prometheus.HistogramVec
	atrasoOutbox prometheus.Gauge
	divergencias prometheus.Counter
}

// NewMetrics registra as métricas num registro próprio.
//
// Registro próprio, e não o global: coletores globais vazam entre testes e
// tornam impossível ter duas instâncias no mesmo processo.
func NewMetrics() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		registro: r,
		resultados: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wagering_transactions_total",
			Help: "Operações por tipo, estado final e origem.",
		}, []string{"kind", "status", "source"}),
		duplicatas: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wagering_duplicates_total",
			Help: "Operações reconhecidas como repetição.",
		}, []string{"source"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wagering_retries_total",
			Help: "Novas tentativas por componente.",
		}, []string{"component"}),
		dlq: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wagering_dead_letter_total",
			Help: "Mensagens e eventos enviados à dead-letter.",
		}, []string{"component"}),
		conflitos: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wallet_lock_conflicts_total",
			Help: "Conflitos de concorrência que exigiram refazer a transação.",
		}),
		latencia: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "wagering_processing_seconds",
			Help:    "Tempo de processamento de uma operação.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"source"}),
		atrasoOutbox: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "outbox_pending_age_seconds",
			Help: "Idade do evento pendente mais antigo. Cresce quando a publicação para.",
		}),
		divergencias: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reconciliation_divergences_total",
			Help: "Reconciliações em que saldo e ledger não bateram.",
		}),
	}
	r.MustRegister(m.resultados, m.duplicatas, m.retries, m.dlq,
		m.conflitos, m.latencia, m.atrasoOutbox, m.divergencias)
	return m
}

func (m *Metrics) TransactionResult(kind, status, source string) {
	m.resultados.WithLabelValues(kind, status, source).Inc()
}
func (m *Metrics) Duplicate(source string)     { m.duplicatas.WithLabelValues(source).Inc() }
func (m *Metrics) Retry(component string)      { m.retries.WithLabelValues(component).Inc() }
func (m *Metrics) DeadLetter(component string) { m.dlq.WithLabelValues(component).Inc() }
func (m *Metrics) ConcurrencyConflict()        { m.conflitos.Inc() }
func (m *Metrics) ProcessingTime(source string, d time.Duration) {
	m.latencia.WithLabelValues(source).Observe(d.Seconds())
}
func (m *Metrics) OutboxLag(d time.Duration) { m.atrasoOutbox.Set(d.Seconds()) }
func (m *Metrics) ReconciliationDivergence() { m.divergencias.Inc() }

// Handler expõe /metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registro, promhttp.HandlerOpts{})
}
