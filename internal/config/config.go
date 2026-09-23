// Package config lê e valida a configuração do ambiente.
//
// A validação acontece na inicialização, não no primeiro uso: um serviço que
// sobe com configuração inválida e só falha na primeira aposta é pior que um
// que se recusa a subir.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config é a configuração completa do serviço.
type Config struct {
	Env      string
	HTTPAddr string

	DatabaseURL        string
	DBMaxConns         int32
	DBMinConns         int32
	DBLockTimeout      time.Duration
	DBStatementTimeout time.Duration

	AWSRegion          string
	SQSEndpoint        string
	AWSAccessKey       string
	AWSSecretKey       string
	QueueIn            string
	QueueDLQ           string
	QueueOut           string
	SQSMaxReceive      int
	SQSSenderProviders map[string]string

	OIDCIssuer        string
	OIDCAudience      string
	OIDCInternalScope string

	OutboxInterval    time.Duration
	OutboxBatch       int
	OutboxMaxAttempts int

	PendingInterval    time.Duration
	PendingBatch       int
	PendingBaseDelay   time.Duration
	PendingMaxDelay    time.Duration
	PendingMaxAttempts int

	ConflictRetries int
	LogLevel        string
	ShutdownTimeout time.Duration
}

// Load lê a configuração do ambiente e a valida.
func Load() (Config, error) {
	c := Config{
		Env:      texto("APP_ENV", "development"),
		HTTPAddr: texto("HTTP_ADDR", ":8080"),

		DatabaseURL:        texto("DATABASE_URL", ""),
		DBMaxConns:         int32(inteiro("DB_MAX_CONNS", 10)),
		DBMinConns:         int32(inteiro("DB_MIN_CONNS", 1)),
		DBLockTimeout:      duracao("DB_LOCK_TIMEOUT", 5*time.Second),
		DBStatementTimeout: duracao("DB_STATEMENT_TIMEOUT", 30*time.Second),

		AWSRegion:          texto("AWS_REGION", "us-east-1"),
		SQSEndpoint:        texto("SQS_ENDPOINT", ""),
		AWSAccessKey:       texto("AWS_ACCESS_KEY_ID", ""),
		AWSSecretKey:       texto("AWS_SECRET_ACCESS_KEY", ""),
		QueueIn:            texto("SQS_QUEUE_IN", "wager-transactions.fifo"),
		QueueDLQ:           texto("SQS_QUEUE_DLQ", "wager-transactions-dlq.fifo"),
		QueueOut:           texto("SQS_QUEUE_OUT", "wager-events.fifo"),
		SQSMaxReceive:      inteiro("SQS_MAX_RECEIVE_COUNT", 5),
		SQSSenderProviders: mapa("SQS_SENDER_PROVIDERS"),

		OIDCIssuer:        texto("OIDC_ISSUER_URL", ""),
		OIDCAudience:      texto("OIDC_AUDIENCE", ""),
		OIDCInternalScope: texto("OIDC_INTERNAL_SCOPE", "wallets:write"),

		OutboxInterval:    duracao("OUTBOX_INTERVAL", time.Second),
		OutboxBatch:       inteiro("OUTBOX_BATCH", 20),
		OutboxMaxAttempts: inteiro("OUTBOX_MAX_ATTEMPTS", 8),

		PendingInterval:    duracao("PENDING_INTERVAL", 2*time.Second),
		PendingBatch:       inteiro("PENDING_BATCH", 20),
		PendingBaseDelay:   duracao("PENDING_BASE_DELAY", time.Second),
		PendingMaxDelay:    duracao("PENDING_MAX_DELAY", time.Minute),
		PendingMaxAttempts: inteiro("PENDING_MAX_ATTEMPTS", 6),

		ConflictRetries: inteiro("CONFLICT_RETRIES", 3),
		LogLevel:        texto("LOG_LEVEL", "info"),
		ShutdownTimeout: duracao("SHUTDOWN_TIMEOUT", 20*time.Second),
	}
	return c, c.Validate()
}

// Validate reúne TODOS os problemas antes de falhar.
//
// Reportar um erro por vez faria quem está configurando o serviço descobrir
// os problemas em série, com um deploy a cada descoberta.
func (c Config) Validate() error {
	var problemas []string
	if c.DatabaseURL == "" {
		problemas = append(problemas, "DATABASE_URL é obrigatória")
	}
	if c.OIDCIssuer == "" {
		problemas = append(problemas, "OIDC_ISSUER_URL é obrigatória: a autenticação não é opcional")
	}
	if c.DBMaxConns < c.DBMinConns {
		problemas = append(problemas, "DB_MAX_CONNS não pode ser menor que DB_MIN_CONNS")
	}
	if c.PendingMaxDelay < c.PendingBaseDelay {
		problemas = append(problemas, "PENDING_MAX_DELAY não pode ser menor que PENDING_BASE_DELAY")
	}
	if c.SQSMaxReceive < 1 {
		problemas = append(problemas, "SQS_MAX_RECEIVE_COUNT precisa ser ao menos 1")
	}
	if len(problemas) > 0 {
		return fmt.Errorf("configuração inválida:\n  - %s", strings.Join(problemas, "\n  - "))
	}
	return nil
}

// Production informa se o serviço roda em produção.
func (c Config) Production() bool { return c.Env == "production" }

var errAusente = errors.New("variável ausente")

func texto(chave, padrao string) string {
	if v := strings.TrimSpace(os.Getenv(chave)); v != "" {
		return v
	}
	return padrao
}

func inteiro(chave string, padrao int) int {
	v := os.Getenv(chave)
	if v == "" {
		return padrao
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return padrao
	}
	return n
}

func duracao(chave string, padrao time.Duration) time.Duration {
	v := os.Getenv(chave)
	if v == "" {
		return padrao
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return padrao
	}
	return d
}

// mapa lê pares "chave=valor" separados por vírgula.
// Exemplo: SQS_SENDER_PROVIDERS="000000000000=*,arn:aws:iam::1:user/a=provider-a"
func mapa(chave string) map[string]string {
	v := os.Getenv(chave)
	if v == "" {
		return nil
	}
	m := map[string]string{}
	for _, par := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(strings.TrimSpace(par), "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	return m
}

var _ = errAusente
