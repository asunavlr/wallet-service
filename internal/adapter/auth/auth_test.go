package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kevinmatos/wallet-service/internal/adapter/auth"
)

// idpFalso é um IdP de verdade em miniatura: chave RSA própria, JWKS servido
// e tokens realmente assinados.
//
// Não é um mock do verificador — o verificador sob teste faz a validação
// criptográfica completa contra esta chave. O que se dispensa é o Keycloak,
// que a suíte e2e cobre.
type idpFalso struct {
	*httptest.Server
	chave *rsa.PrivateKey
	kid   string
	// emissorAnunciado é o `issuer` do documento de descoberta, que pode
	// diferir do endereço por onde o serviço alcança o IdP.
	emissorAnunciado string
	// jwksExterno simula o jwks_uri anunciado com o nome de fora da rede.
	jwksExterno string
}

func novoIdP(t *testing.T) *idpFalso {
	t.Helper()
	chave, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &idpFalso{chave: chave, kid: "chave-de-teste"}

	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	if idp.emissorAnunciado == "" {
		idp.emissorAnunciado = idp.URL
	}

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		jwks := idp.jwksExterno
		if jwks == "" {
			jwks = idp.URL + "/jwks"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.emissorAnunciado,
			"jwks_uri":                              jwks,
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(chave.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(chave.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": idp.kid, "n": n, "e": e},
			},
		})
	})
	return idp
}

// token assina um JWT com os claims dados.
func (i *idpFalso) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	cabecalho := map[string]any{"alg": "RS256", "typ": "JWT", "kid": i.kid}
	seg := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	corpo := seg(cabecalho) + "." + seg(claims)
	assinatura, err := assinarRS256(i.chave, corpo)
	if err != nil {
		t.Fatal(err)
	}
	return corpo + "." + assinatura
}

func (i *idpFalso) claimsPadrao(extras map[string]any) map[string]any {
	c := map[string]any{
		"iss": i.emissorAnunciado,
		"sub": "cliente-de-teste",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extras {
		c[k] = v
	}
	return c
}

func verificador(t *testing.T, i *idpFalso, cfg auth.Config) *auth.Verifier {
	t.Helper()
	if cfg.IssuerURL == "" {
		cfg.IssuerURL = i.emissorAnunciado
	}
	if cfg.DiscoveryURL == "" {
		cfg.DiscoveryURL = i.URL
	}
	v, err := auth.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("montando o verificador: %v", err)
	}
	return v
}

func TestTokenValidoEAceito(t *testing.T) {
	idp := novoIdP(t)
	v := verificador(t, idp, auth.Config{})

	tok := idp.token(t, idp.claimsPadrao(map[string]any{"provider_id": "provider-a"}))
	claims, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("token válido recusado: %v", err)
	}
	if claims.ProviderID != "provider-a" {
		t.Errorf("provider_id = %q", claims.ProviderID)
	}
	if claims.Subject != "cliente-de-teste" {
		t.Errorf("sub = %q", claims.Subject)
	}
}

// Credencial ausente, inválida ou expirada: todas recusadas, e todas com o
// mesmo erro — detalhar qual checagem falhou ajuda quem tenta forjar token.
func TestTokensRecusados(t *testing.T) {
	idp := novoIdP(t)
	outro := novoIdP(t)
	v := verificador(t, idp, auth.Config{})

	casos := map[string]string{
		"vazio":              "",
		"lixo":               "não-é-um-jwt",
		"três partes falsas": "aaa.bbb.ccc",
		"expirado": idp.token(t, idp.claimsPadrao(map[string]any{
			"exp": time.Now().Add(-time.Minute).Unix(),
		})),
		"emissor errado": idp.token(t, idp.claimsPadrao(map[string]any{
			"iss": "https://outro-emissor.example",
		})),
		"assinado por outra chave": outro.token(t, map[string]any{
			"iss": idp.emissorAnunciado,
			"sub": "invasor",
			"exp": time.Now().Add(time.Hour).Unix(),
		}),
	}
	for nome, tok := range casos {
		t.Run(nome, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tok); err == nil {
				t.Error("deveria ser recusado")
			} else if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Errorf("erro = %v, queria ErrUnauthenticated", err)
			}
		})
	}
}

// Assinatura adulterada precisa cair: é o ponto do JWT.
func TestAssinaturaAdulterada(t *testing.T) {
	idp := novoIdP(t)
	v := verificador(t, idp, auth.Config{})

	tok := idp.token(t, idp.claimsPadrao(map[string]any{"provider_id": "provider-a"}))
	partes := strings.Split(tok, ".")

	// troca o provider_id no corpo, mantendo a assinatura original
	var claims map[string]any
	corpo, _ := base64.RawURLEncoding.DecodeString(partes[1])
	_ = json.Unmarshal(corpo, &claims)
	claims["provider_id"] = "provider-b"
	novo, _ := json.Marshal(claims)
	forjado := partes[0] + "." + base64.RawURLEncoding.EncodeToString(novo) + "." + partes[2]

	if _, err := v.Verify(context.Background(), forjado); err == nil {
		t.Fatal("FALHA DE SEGURANÇA: token com corpo adulterado foi aceito")
	}
}

// alg=none é o ataque clássico contra validadores permissivos.
func TestAlgNoneERecusado(t *testing.T) {
	idp := novoIdP(t)
	v := verificador(t, idp, auth.Config{})

	seg := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	tok := seg(map[string]any{"alg": "none", "typ": "JWT"}) + "." +
		seg(idp.claimsPadrao(map[string]any{"provider_id": "provider-a"})) + "."

	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("FALHA DE SEGURANÇA: token com alg=none foi aceito")
	}
}

// O IdP visto por nomes diferentes de dentro e de fora da rede: a descoberta
// acontece pelo endereço interno e o jwks_uri anunciado é reescrito para ele.
// A validação do `iss` continua estrita.
func TestEmissorExternoComDescobertaInterna(t *testing.T) {
	idp := novoIdP(t)
	idp.emissorAnunciado = "https://auth.externo.example/realms/wallet"
	// O IdP anuncia o JWKS no nome EXTERNO, inalcançável daqui. O caminho é o
	// mesmo dos dois lados — só o host muda —, que é como um IdP real se
	// comporta atrás de proxy ou rede de container.
	idp.jwksExterno = "https://auth.externo.example/jwks"

	v := verificador(t, idp, auth.Config{
		IssuerURL:    "https://auth.externo.example/realms/wallet",
		DiscoveryURL: idp.URL,
	})

	tok := idp.token(t, idp.claimsPadrao(map[string]any{"provider_id": "provider-a"}))
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("token do emissor externo recusado: %v", err)
	}

	// e um token do emissor ERRADO continua caindo
	errado := idp.token(t, map[string]any{
		"iss": idp.URL, "sub": "x", "exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := v.Verify(context.Background(), errado); err == nil {
		t.Error("token com emissor divergente deveria ser recusado")
	}
}

// Emissor anunciado diferente do configurado é erro de CONFIGURAÇÃO: falha na
// montagem, não em silêncio na primeira requisição.
func TestEmissorAnunciadoDivergenteFalhaNaMontagem(t *testing.T) {
	idp := novoIdP(t)
	_, err := auth.New(context.Background(), auth.Config{
		IssuerURL:    "https://esperado.example",
		DiscoveryURL: idp.URL,
	})
	if err == nil {
		t.Fatal("deveria recusar: o IdP anuncia outro emissor")
	}
	if !errors.Is(err, auth.ErrDescoberta) {
		t.Errorf("erro = %v, queria ErrDescoberta", err)
	}
}

func TestIdPInacessivelFalhaNaMontagem(t *testing.T) {
	_, err := auth.New(context.Background(), auth.Config{
		IssuerURL: "http://127.0.0.1:1/realms/x",
	})
	if !errors.Is(err, auth.ErrDescoberta) {
		t.Errorf("erro = %v, queria ErrDescoberta", err)
	}
}

// O escopo interno identifica o serviço que pode operar carteira.
func TestEscopoInterno(t *testing.T) {
	idp := novoIdP(t)
	v := verificador(t, idp, auth.Config{InternalScope: "wallets:write"})

	casos := map[string]bool{
		"wallets:write":                true,
		"openid profile wallets:write": true,
		"wallets:write openid":         true,
		"":                             false,
		"openid profile":               false,
		"wallets:writeX":               false,
		"xwallets:write":               false,
	}
	for escopo, quer := range casos {
		t.Run(fmt.Sprintf("%q", escopo), func(t *testing.T) {
			if got := v.Internal(auth.Claims{Scope: escopo}); got != quer {
				t.Errorf("Internal(%q) = %v, queria %v", escopo, got, quer)
			}
		})
	}
}

func TestBearer(t *testing.T) {
	validos := map[string]string{
		"Bearer abc":  "abc",
		"bearer abc":  "abc",
		"BEARER abc":  "abc",
		"Bearer  abc": "abc",
	}
	for header, quer := range validos {
		got, err := auth.Bearer(header)
		if err != nil || got != quer {
			t.Errorf("Bearer(%q) = %q, %v", header, got, err)
		}
	}
	for _, header := range []string{"", "abc", "Basic abc", "Bearer", "Bearer ", "BearerX abc"} {
		if _, err := auth.Bearer(header); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("Bearer(%q) deveria ser recusado", header)
		}
	}
}

// A reescrita do jwks_uri preserva o CAMINHO e troca só o host. Um IdP pode
// servir as chaves em qualquer caminho, e inventar um fixo quebraria contra
// qualquer um que não seja o do Keycloak.
func TestReescritaDoJWKSPreservaOCaminho(t *testing.T) {
	idp := novoIdP(t)
	idp.emissorAnunciado = "https://externo.example/auth/realms/wallet"
	idp.jwksExterno = "https://externo.example/auth/realms/wallet/protocol/openid-connect/certs"

	// o IdP falso passa a servir as chaves exatamente nesse caminho
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.emissorAnunciado,
			"jwks_uri":                              idp.jwksExterno,
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	var servido bool
	mux.HandleFunc("/auth/realms/wallet/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		servido = true
		n := base64.RawURLEncoding.EncodeToString(idp.chave.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(idp.chave.E)).Bytes())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{
				{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": idp.kid, "n": n, "e": e},
			},
		})
	})
	idp.Config.Handler = mux

	v := verificador(t, idp, auth.Config{
		IssuerURL:    idp.emissorAnunciado,
		DiscoveryURL: idp.URL,
	})
	if _, err := v.Verify(context.Background(), idp.token(t, idp.claimsPadrao(nil))); err != nil {
		t.Fatalf("verificação falhou: %v", err)
	}
	if !servido {
		t.Error("o JWKS deveria ter sido buscado no caminho anunciado, com o host interno")
	}
}
