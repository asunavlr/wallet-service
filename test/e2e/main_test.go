//go:build e2e

// Package e2e_test exercita o serviço COMPLETO pela rede, como um provedor
// de jogos faria: token do Keycloak de verdade, HTTP de verdade, três
// instâncias atrás do mesmo banco.
//
// Roda contra o ambiente do docker-compose:
//
//	docker compose up -d --build
//	go test -tags e2e ./test/e2e/ -v
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// As três instâncias. Usar as três nos testes é o que demonstra as garantias
// "com pelo menos três processos independentes" que o enunciado exige.
func instancias() []string {
	if v := os.Getenv("E2E_BASE_URLS"); v != "" {
		return strings.Split(v, ",")
	}
	return []string{"http://localhost:8080", "http://localhost:8082", "http://localhost:8083"}
}

func keycloak() string {
	if v := os.Getenv("E2E_KEYCLOAK_URL"); v != "" {
		return v
	}
	return "http://localhost:8081"
}

var cliente = &http.Client{Timeout: 15 * time.Second}

// token obtém um access token por client_credentials, como um serviço faz.
func token(t *testing.T, clientID, secret string) string {
	t.Helper()
	dados := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
	}
	endpoint := keycloak() + "/realms/wallet/protocol/openid-connect/token"
	resp, err := cliente.Post(endpoint, "application/x-www-form-urlencoded", strings.NewReader(dados.Encode()))
	if err != nil {
		t.Skipf("Keycloak indisponível em %s: %v", keycloak(), err)
	}
	defer resp.Body.Close()
	corpo, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token para %s = %d: %s", clientID, resp.StatusCode, corpo)
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(corpo, &out); err != nil {
		t.Fatal(err)
	}
	return out.AccessToken
}

type resposta struct {
	Status int
	Corpo  map[string]any
	Bruto  string
}

func chamar(t *testing.T, base, metodo, caminho, tok string, corpo any, headers map[string]string) resposta {
	t.Helper()
	var leitor io.Reader
	if corpo != nil {
		b, _ := json.Marshal(corpo)
		leitor = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), metodo, base+caminho, leitor)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if corpo != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cliente.Do(req)
	if err != nil {
		t.Skipf("serviço indisponível em %s: %v", base, err)
	}
	defer resp.Body.Close()
	bruto, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(bruto, &m)
	return resposta{Status: resp.StatusCode, Corpo: m, Bruto: string(bruto)}
}

// chamarSemPular é chamar() que NÃO pula o teste quando a conexão falha: um
// erro de rede durante um encerramento é justamente o que se quer observar.
func chamarSemPular(base, metodo, caminho, tok string, corpo any, headers map[string]string) resposta {
	var leitor io.Reader
	if corpo != nil {
		b, _ := json.Marshal(corpo)
		leitor = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), metodo, base+caminho, leitor)
	if err != nil {
		return resposta{}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if corpo != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := cliente.Do(req)
	if err != nil {
		return resposta{} // Status 0: conexão recusada ou cortada
	}
	defer resp.Body.Close()
	bruto, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(bruto, &m)
	return resposta{Status: resp.StatusCode, Corpo: m, Bruto: string(bruto)}
}

func dinheiro(v string) map[string]string {
	return map[string]string{"amount": v, "currency": "BRL"}
}

func esperar(t *testing.T, cond func() bool, prazo time.Duration, oque string) {
	t.Helper()
	limite := time.Now().Add(prazo)
	for time.Now().Before(limite) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("tempo esgotado esperando: %s", oque)
}

// sufixo gera um identificador único para os dados de um teste.
//
// NÃO derive isso de um pedaço do UUID da carteira: o UUIDv7 é ordenado por
// tempo, então os primeiros dígitos são iguais para tudo que nasce no mesmo
// instante — e ids de teste colidiriam entre execuções, fazendo a inbox
// deduplicar corretamente uma mensagem que o teste achava ser nova.
func sufixo() string {
	return uuid.NewString()[24:] // os 12 dígitos finais, que são aleatórios
}

// uuidNovo gera um UUIDv7, como o serviço faz.
func uuidNovo() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
