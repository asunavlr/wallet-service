package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Health responde pelos health checks.
type Health interface {
	// Live indica que o processo está vivo. Não toca em dependência: um
	// liveness que depende do banco derruba o processo quando o banco pisca.
	Live(ctx context.Context) error
	// Ready indica que as dependências respondem: Postgres e SQS.
	Ready(ctx context.Context) error
}

// Router monta as rotas.
func Router(h *Handlers, a Autenticador, saude Health, log *slog.Logger, metricas http.Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.Recoverer)
	r.Use(Correlacionar)
	r.Use(Registrar(log))

	// Health checks são públicos: um probe de orquestrador não carrega token.
	r.Get("/health/live", func(w http.ResponseWriter, r *http.Request) {
		if err := saude.Live(r.Context()); err != nil {
			escreverJSON(w, http.StatusServiceUnavailable, Problem{Code: "NOT_LIVE", Message: err.Error()})
			return
		}
		escreverJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	r.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := saude.Ready(ctx); err != nil {
			escreverJSON(w, http.StatusServiceUnavailable, Problem{Code: "NOT_READY", Message: err.Error()})
			return
		}
		escreverJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	if metricas != nil {
		r.Method(http.MethodGet, "/metrics", metricas)
	}

	// Tudo que é negócio exige autenticação.
	r.Group(func(r chi.Router) {
		r.Use(Autenticar(a))

		// TODA operação de carteira é interna, e não só a abertura.
		//
		// O enunciado diz, na seção de autorização, que "operações de
		// carteira são restritas ao serviço interno". Ler é uma delas: uma
		// carteira pode ser movimentada por vários provedores, e o ledger
		// expõe valor e identificador das operações de todos eles. Deixar a
		// leitura aberta permitia a um provedor ver quanto o jogador apostou
		// no concorrente — que é exatamente o "acesso não autorizado a
		// operações" que o enunciado trata como eliminatório.
		//
		// O provedor continua vendo o saldo que as PRÓPRIAS operações
		// produziram: ele vem na resposta de cada operação e no replay.
		r.Group(func(r chi.Router) {
			r.Use(SomenteInterno)
			r.Post("/wallets", h.AbrirCarteira)
			r.Get("/wallets/{walletId}", h.LerCarteira)
			r.Get("/wallets/{walletId}/ledger", h.LerLedger)
			r.Post("/wallets/{walletId}/reconciliation", h.Reconciliar)
		})

		r.Post("/wagering/transactions", h.EnviarOperacao)
		r.Get("/wagering/transactions/{transactionId}", h.LerOperacao)
		r.Get("/providers/{providerId}/wagering/transactions/{externalTransactionId}", h.LerOperacaoPorExterno)
	})

	return r
}

// Server embrulha o http.Server com o ciclo de vida que o Fx gerencia.
type Server struct {
	http *http.Server
	log  *slog.Logger
}

// NewServer monta o servidor.
func NewServer(addr string, h http.Handler, log *slog.Logger) *Server {
	return &Server{
		http: &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		log: log,
	}
}

// Start sobe o servidor sem bloquear.
func (s *Server) Start() error {
	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("servidor http parou", slog.String("erro", err.Error()))
		}
	}()
	s.log.Info("servidor http ouvindo", slog.String("addr", s.http.Addr))
	return nil
}

// Stop interrompe novas entradas e conclui o que está em andamento dentro do
// prazo do contexto, que é o que o enunciado pede para o shutdown.
func (s *Server) Stop(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// Addr devolve o endereço de escuta.
func (s *Server) Addr() string { return s.http.Addr }
