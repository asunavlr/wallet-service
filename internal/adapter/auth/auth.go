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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	// ErrUnauthenticated indica credencial ausente, inválida ou expirada.
	ErrUnauthenticated = errors.New("auth: não autenticado")
	// ErrDescoberta indica falha ao obter a configuração do IdP.
	ErrDescoberta = errors.New("auth: descoberta do IdP")
)

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
	// IssuerURL é o que os tokens declaram em `iss`, e é contra ele que a
	// validação acontece.
	IssuerURL string
	// DiscoveryURL é o endereço por onde ESTE processo alcança o IdP na
	// rede. Normalmente igual a IssuerURL; difere quando o IdP é visto por
	// nomes distintos de dentro e de fora da rede — o caso do container
	// (`http://keycloak:8080`) contra o navegador (`http://localhost:8081`),
	// e também o de um service mesh com emissor público.
	//
	// A validação continua estrita: o `iss` do token precisa bater com
	// IssuerURL. O que muda é apenas ONDE o JWKS é buscado.
	DiscoveryURL  string
	Audience      string
	InternalScope string
}

// New monta o verificador buscando a configuração do IdP.
//
// O JWKS é obtido e mantido em cache pela biblioteca, com renovação
// automática: a validação de assinatura não faz ida e volta ao Keycloak a
// cada requisição, mas acompanha rotação de chave.
func New(ctx context.Context, c Config) (*Verifier, error) {
	cfg := &oidc.Config{
		ClientID:             c.Audience,
		SupportedSigningAlgs: []string{oidc.RS256},
	}
	if c.Audience == "" {
		// Sem audience configurada, a checagem é desligada explicitamente,
		// em vez de aceitar qualquer valor por omissão.
		cfg.SkipClientIDCheck = true
	}
	escopo := c.InternalScope
	if escopo == "" {
		escopo = "wallets:write"
	}

	jwks, err := enderecoJWKS(ctx, c)
	if err != nil {
		return nil, err
	}
	// O emissor exigido é sempre c.IssuerURL: a validação do `iss` de cada
	// token não afrouxa. O que muda é apenas de ONDE as chaves vêm.
	verificador := oidc.NewVerifier(c.IssuerURL, oidc.NewRemoteKeySet(ctx, jwks), cfg)
	return &Verifier{verificador: verificador, escopoInterno: escopo}, nil
}

// enderecoJWKS descobre onde buscar as chaves públicas.
//
// Quando o IdP é visto por nomes diferentes de dentro e de fora da rede, o
// documento de descoberta anuncia o `jwks_uri` com o nome EXTERNO — que é o
// correto para um navegador e inalcançável de dentro de um container. Aqui a
// descoberta é feita pelo endereço interno e o host do `jwks_uri` é reescrito
// para esse mesmo endereço, preservando o caminho que o IdP indicou.
//
// Isso não é particularidade de Docker: acontece igual com service mesh, onde
// o emissor é público e o serviço fala com o IdP por um endereço interno.
func enderecoJWKS(ctx context.Context, c Config) (string, error) {
	descoberta := c.DiscoveryURL
	if descoberta == "" {
		descoberta = c.IssuerURL
	}

	doc, err := baixarDescoberta(ctx, descoberta)
	if err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("%w: o IdP não anunciou jwks_uri", ErrDescoberta)
	}
	// O emissor anunciado precisa ser o que exigimos dos tokens; divergência
	// aqui significaria validar contra um emissor que não é o do IdP.
	if doc.Issuer != c.IssuerURL {
		return "", fmt.Errorf("%w: o IdP anuncia o emissor %q, mas a configuração exige %q",
			ErrDescoberta, doc.Issuer, c.IssuerURL)
	}
	if descoberta == c.IssuerURL {
		return doc.JWKSURI, nil
	}

	anunciado, err := url.Parse(doc.JWKSURI)
	if err != nil {
		return "", fmt.Errorf("%w: jwks_uri inválido: %s", ErrDescoberta, err)
	}
	interno, err := url.Parse(descoberta)
	if err != nil {
		return "", fmt.Errorf("%w: OIDC_DISCOVERY_URL inválida: %s", ErrDescoberta, err)
	}
	anunciado.Scheme, anunciado.Host = interno.Scheme, interno.Host
	return anunciado.String(), nil
}

type documentoOIDC struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

func baixarDescoberta(ctx context.Context, base string) (documentoOIDC, error) {
	endereco := strings.TrimSuffix(base, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endereco, nil)
	if err != nil {
		return documentoOIDC{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return documentoOIDC{}, fmt.Errorf("%w: %s inacessível: %s", ErrDescoberta, endereco, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return documentoOIDC{}, fmt.Errorf("%w: %s devolveu %d", ErrDescoberta, endereco, resp.StatusCode)
	}
	var doc documentoOIDC
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return documentoOIDC{}, fmt.Errorf("%w: resposta ilegível de %s: %s", ErrDescoberta, endereco, err)
	}
	return doc, nil
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
