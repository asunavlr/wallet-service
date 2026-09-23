package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kevinmatos/wallet-service/internal/adapter/auth"
)

// Autenticador é o que o middleware precisa saber sobre tokens.
type Autenticador interface {
	Verify(ctx context.Context, bruto string) (auth.Claims, error)
	Internal(c auth.Claims) bool
}

// Autenticar exige um token válido e coloca a identidade no contexto.
//
// Sem token válido, nada acontece: a requisição é recusada antes de chegar ao
// handler, então não há efeito financeiro nem exposição de dado num acesso
// não autorizado — que é o que o enunciado exige verificar.
func Autenticar(a Autenticador) func(http.Handler) http.Handler {
	return func(proximo http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			bruto, err := auth.Bearer(r.Header.Get("Authorization"))
			if err != nil {
				naoAutenticado(w)
				return
			}
			claims, err := a.Verify(r.Context(), bruto)
			if err != nil {
				naoAutenticado(w)
				return
			}
			ident := Identity{
				Subject: claims.Subject, ProviderID: claims.ProviderID,
				Internal: a.Internal(claims),
			}
			proximo.ServeHTTP(w, r.WithContext(ComIdentidade(r.Context(), ident)))
		})
	}
}

// SomenteInterno restringe a rota ao serviço interno.
func SomenteInterno(proximo http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := exigirInterno(r.Context()); err != nil {
			escreverErro(w, err)
			return
		}
		proximo.ServeHTTP(w, r)
	})
}

func naoAutenticado(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="wallet"`)
	escreverJSON(w, http.StatusUnauthorized, Problem{
		Code: "UNAUTHENTICATED", Message: "credencial ausente, inválida ou expirada",
	})
}

// Correlacionar garante um identificador de correlação por requisição.
//
// Se o cliente mandou o dele, respeitamos: é o que permite seguir um pedido
// por vários serviços. Senão, geramos.
func Correlacionar(proximo http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Correlation-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Correlation-Id", id)
		proximo.ServeHTTP(w, r.WithContext(ComCorrelation(r.Context(), id)))
	})
}

// Registrar emite uma linha de log estruturada por requisição.
//
// Registra os identificadores que permitem rastrear a operação e NÃO registra
// credencial nem payload financeiro completo — o enunciado proíbe os dois.
func Registrar(log *slog.Logger) func(http.Handler) http.Handler {
	return func(proximo http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inicio := time.Now()
			rec := &respostaGravada{ResponseWriter: w, status: http.StatusOK}
			proximo.ServeHTTP(rec, r)

			atributos := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("elapsed", time.Since(inicio)),
				slog.String("correlationId", CorrelationID(r.Context())),
			}
			if ident, ok := IdentidadeDe(r.Context()); ok && ident.ProviderID != "" {
				atributos = append(atributos, slog.String("providerId", ident.ProviderID))
			}
			log.LogAttrs(r.Context(), nivelPara(rec.status), "requisição http", paraAttrs(atributos)...)
		})
	}
}

func nivelPara(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func paraAttrs(vs []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(vs))
	for _, v := range vs {
		if a, ok := v.(slog.Attr); ok {
			attrs = append(attrs, a)
		}
	}
	return attrs
}

type respostaGravada struct {
	http.ResponseWriter
	status int
}

func (r *respostaGravada) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
