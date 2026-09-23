package httpapi

import (
	"context"
	"fmt"

	"github.com/kevinmatos/wallet-service/internal/app"
)

type chaveContexto int

const (
	chaveCorrelation chaveContexto = iota
	chaveIdentidade
)

// Identity é quem está chamando, derivado do token.
type Identity struct {
	Subject string
	// ProviderID vem do claim do token. É ele que manda: o providerId do
	// corpo da requisição é conferido contra este, nunca o contrário.
	ProviderID string
	// Internal marca o serviço interno, único autorizado a abrir carteira.
	Internal bool
}

// ComCorrelation devolve um contexto com o identificador de correlação.
func ComCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, chaveCorrelation, id)
}

// CorrelationID lê o identificador de correlação.
func CorrelationID(ctx context.Context) string {
	if v, ok := ctx.Value(chaveCorrelation).(string); ok {
		return v
	}
	return ""
}

// ComIdentidade devolve um contexto com a identidade autenticada.
func ComIdentidade(ctx context.Context, i Identity) context.Context {
	return context.WithValue(ctx, chaveIdentidade, i)
}

// IdentidadeDe lê a identidade autenticada.
func IdentidadeDe(ctx context.Context) (Identity, bool) {
	i, ok := ctx.Value(chaveIdentidade).(Identity)
	return i, ok
}

// exigirProvedor confere que a identidade pode agir por aquele provedor.
//
// É a regra que o enunciado trata como eliminatória: "a identidade
// autenticada deve determinar o providerId autorizado". O serviço interno
// passa por qualquer provedor porque é ele que opera o worker de pendências,
// que reavalia operações de todos.
func exigirProvedor(ctx context.Context, providerID string) error {
	ident, ok := IdentidadeDe(ctx)
	if !ok {
		return fmt.Errorf("%w: requisição não autenticada", app.ErrForbidden)
	}
	if ident.Internal {
		return nil
	}
	if ident.ProviderID == "" || ident.ProviderID != providerID {
		return fmt.Errorf("%w: identidade não autorizada para o provedor %q", app.ErrForbidden, providerID)
	}
	return nil
}

// exigirInterno restringe a operação ao serviço interno.
func exigirInterno(ctx context.Context) error {
	ident, ok := IdentidadeDe(ctx)
	if !ok {
		return fmt.Errorf("%w: requisição não autenticada", app.ErrForbidden)
	}
	if !ident.Internal {
		return fmt.Errorf("%w: operação restrita ao serviço interno", app.ErrForbidden)
	}
	return nil
}
