// Package auth valida tokens do IdP externo.
//
// Escolha do IdP: Keycloak, por ser o recomendado pelo enunciado, rodar em
// container sem dependência de nuvem e permitir provisionamento automático do
// realm por arquivo — o que mantém o `docker compose up` reprodutível.
//
// Fluxo: client_credentials. Não há usuário final nesta API; quem chama é um
// serviço (o provedor de jogos ou o próprio serviço interno). Cadastro de
// senha e emissão de token estão fora do escopo, como o enunciado define.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// ErrUnauthenticated indica credencial ausente, inválida ou expirada.
var ErrUnauthenticated = errors.New("auth: não autenticado")

// Claims são os campos que o serviço lê do token.
type Claims struct {
	Subject string `json:"sub"`
	// ProviderID é o claim que amarra o token a um provedor de jogos.
	// Configurado no Keycloak como atributo do client.
	ProviderID string `json:"provider_id"`
	// Scope carrega os escopos concedidos; "wallets:write" identifica o
	// serviço interno.
	Scope string `json:"scope"`
}

// Verifier valida tokens contra o IdP.
type Verifier struct {
	verificador   *oidc.IDTokenVerifier
	escopoInterno string
}

// Config é a configuração do verificador.
type Config struct {
	IssuerURL     string
	Audience      string
	InternalScope string
}

// New monta o verificador buscando a configuração do IdP.
//
// O JWKS é obtido e mantido em cache pela biblioteca, com renovação
// automática: a validação de assinatura não faz ida e volta ao Keycloak a
// cada requisição, mas acompanha rotação de chave.
func New(ctx context.Context, c Config) (*Verifier, error) {
	provider, err := oidc.NewProvider(ctx, c.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("descobrindo o IdP em %s: %w", c.IssuerURL, err)
	}
	cfg := &oidc.Config{
		ClientID:             c.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	}
	if c.Audience == "" {
		// Sem audience configurada, a checagem é desligada explicitamente em
		// vez de aceitar qualquer valor por omissão.
		cfg.SkipClientIDCheck = true
	}
	escopo := c.InternalScope
	if escopo == "" {
		escopo = "wallets:write"
	}
	return &Verifier{verificador: provider.Verifier(cfg), escopoInterno: escopo}, nil
}

// Verify valida o token e extrai os claims.
//
// Erros de assinatura, emissor, audiência e expiração chegam todos como
// ErrUnauthenticated: o cliente não precisa saber qual das checagens falhou, e
// detalhar ajudaria quem está tentando forjar um token.
func (v *Verifier) Verify(ctx context.Context, bruto string) (Claims, error) {
	token, err := v.verificador.Verify(ctx, bruto)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %s", ErrUnauthenticated, err)
	}
	var c Claims
	if err := token.Claims(&c); err != nil {
		return Claims{}, fmt.Errorf("%w: claims ilegíveis", ErrUnauthenticated)
	}
	if c.Subject == "" {
		c.Subject = token.Subject
	}
	return c, nil
}

// Internal informa se os claims correspondem ao serviço interno.
func (v *Verifier) Internal(c Claims) bool {
	for _, e := range strings.Fields(c.Scope) {
		if e == v.escopoInterno {
			return true
		}
	}
	return false
}

// Bearer extrai o token do header Authorization.
func Bearer(header string) (string, error) {
	const prefixo = "Bearer "
	if len(header) <= len(prefixo) || !strings.EqualFold(header[:len(prefixo)], prefixo) {
		return "", fmt.Errorf("%w: header Authorization ausente ou malformado", ErrUnauthenticated)
	}
	return strings.TrimSpace(header[len(prefixo):]), nil
}
