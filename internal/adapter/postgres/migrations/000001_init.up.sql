-- Schema inicial do serviço de carteira.
--
-- A premissa que organiza este arquivo: com várias instâncias, lock de
-- aplicação não vale nada. Toda invariante financeira que o enunciado exige
-- é imposta aqui — por constraint, índice único, trigger ou privilégio — e
-- continua valendo mesmo que o código da aplicação tenha um bug.

-- ─── carteiras ──────────────────────────────────────────────────────────────

CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor BIGINT      NOT NULL CHECK (balance_minor >= 0),
    version       BIGINT      NOT NULL CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    -- o par (jogador, moeda) identifica uma única carteira
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency)
);

-- Identidade imutável, e versão que anda exatamente junto com o saldo.
-- A regra "incremente apenas quando houver mudança de saldo" é do enunciado, e
-- aqui ela é verificada em vez de confiada ao código: é o que faz LOSS não
-- poder mexer na versão nem por engano.
CREATE FUNCTION wallets_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'carteira % não pode ser removida', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.id, NEW.player_id, NEW.currency, NEW.created_at)
       IS DISTINCT FROM (OLD.id, OLD.player_id, OLD.currency, OLD.created_at) THEN
        RAISE EXCEPTION 'identidade da carteira % é imutável', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.balance_minor <> OLD.balance_minor AND NEW.version <> OLD.version + 1)
       OR (NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version) THEN
        RAISE EXCEPTION 'versão da carteira % deve avançar em 1 exatamente quando o saldo muda', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER wallets_guard BEFORE UPDATE OR DELETE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();

-- ─── transações ─────────────────────────────────────────────────────────────

-- Observação sobre `status`: PENDING não aparece aqui de propósito.
-- O enunciado permite concluir de forma síncrona operações sem dependência
-- ("sem commit intermediário de aceite"), e é o que fazemos: PENDING existe no
-- domínio, em memória, e nunca é commitado. Deixá-lo fora do CHECK transforma
-- "não existe PENDING órfão para retomar" numa garantia do schema, em vez de
-- uma promessa da aplicação. A única espera durável é PENDING_REFERENCE, e
-- essa tem worker próprio.
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    kind                              TEXT        NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status                            TEXT        NOT NULL CHECK (status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),

    wallet_id                         UUID        NOT NULL REFERENCES wallets (id),
    player_id                         UUID        NOT NULL,
    amount_minor                      BIGINT      NOT NULL,
    currency                          CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    -- metadados que só existem na origem externa
    provider_id                       TEXT        CHECK (provider_id <> ''),
    external_transaction_id           TEXT        CHECK (external_transaction_id <> ''),
    idempotency_key                   TEXT        CHECK (idempotency_key <> ''),
    payload_hash                      TEXT        CHECK (payload_hash <> ''),
    round_id                          TEXT        CHECK (round_id <> ''),
    game_id                           TEXT        CHECK (game_id <> ''),
    reference_external_transaction_id TEXT        CHECK (reference_external_transaction_id <> ''),
    reference_transaction_id          UUID        REFERENCES wager_transactions (id),

    -- resultado
    failure_code                      TEXT        CHECK (failure_code <> ''),
    result_balance_minor              BIGINT      CHECK (result_balance_minor >= 0),
    result_currency                   CHAR(3),

    -- agenda do worker de referências pendentes
    attempts                          INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at                   TIMESTAMPTZ,

    correlation_id                    TEXT        NOT NULL DEFAULT '',
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,

    -- Operação interna e externa têm formatos diferentes, e o enunciado pede
    -- que o schema as distinga. OPENING não tem provedor, chave, rodada, jogo
    -- nem referência; uma operação externa tem todos eles.
    CONSTRAINT wager_transactions_origin_shape CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL
            AND idempotency_key IS NULL AND payload_hash IS NULL
            AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL)
        OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL AND game_id IS NOT NULL)
    ),

    -- Política de valor zero: LOSS é exatamente 0.00; todo o resto é positivo.
    CONSTRAINT wager_transactions_zero_policy CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),

    -- REFUND e ROLLBACK exigem referência externa declarada.
    CONSTRAINT wager_transactions_reversal_needs_reference CHECK (
        kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_transaction_id IS NOT NULL
    ),

    -- Uma operação não pode referenciar a si mesma.
    CONSTRAINT wager_transactions_no_self_reference CHECK (
        reference_transaction_id IS NULL OR reference_transaction_id <> id
    ),

    -- Estado terminal de falha sempre tem motivo auditável.
    CONSTRAINT wager_transactions_failure_code CHECK (
        status NOT IN ('REJECTED', 'FAILED') OR failure_code IS NOT NULL
    ),

    -- PROCESSED sempre carrega o saldo observado, porque é ele que o replay
    -- devolve — recalcular na hora do replay daria o saldo errado.
    CONSTRAINT wager_transactions_processed_result CHECK (
        status <> 'PROCESSED' OR result_balance_minor IS NOT NULL
    ),
    CONSTRAINT wager_transactions_result_currency CHECK (
        (result_balance_minor IS NULL) = (result_currency IS NULL)
        AND (result_currency IS NULL OR result_currency ~ '^[A-Z]{3}$')
    ),

    -- Quem espera referência precisa de próxima tentativa agendada, senão a
    -- pendência fica órfã.
    CONSTRAINT wager_transactions_pending_schedule CHECK (
        status <> 'PENDING_REFERENCE' OR next_attempt_at IS NOT NULL
    ),

    -- Alvo da FK composta do ledger (ver ledger_entries_transaction_fkey).
    CONSTRAINT wager_transactions_ledger_target UNIQUE (id, wallet_id, currency, amount_minor)
);

-- Uma carteira tem no máximo um crédito de abertura.
CREATE UNIQUE INDEX wager_transactions_opening_key
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- (providerId, externalTransactionId) identifica uma operação financeira.
-- É esta constraint que impede reaplicar a mesma operação com outra chave.
CREATE UNIQUE INDEX wager_transactions_external_key
    ON wager_transactions (provider_id, external_transaction_id) WHERE origin = 'EXTERNAL';

-- A chave de idempotência tem escopo por provedor: a mesma string vinda de
-- outro provedor é outra operação, e nunca devolve o resultado alheio.
CREATE UNIQUE INDEX wager_transactions_idempotency_key
    ON wager_transactions (provider_id, idempotency_key) WHERE origin = 'EXTERNAL';

-- Uma transação recebe no máximo UMA reversão bem-sucedida, de qualquer tipo.
-- REFUND e ROLLBACK de uma mesma BET devolvem o mesmo débito; permitir os dois
-- seria creditar duas vezes. Ver ARCHITECTURE.md, seção de reversões.
CREATE UNIQUE INDEX wager_transactions_single_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

CREATE INDEX wager_transactions_due_pending
    ON wager_transactions (next_attempt_at) WHERE status = 'PENDING_REFERENCE';

-- Usado para "acordar" as pendências quando a referência delas finalmente chega.
CREATE INDEX wager_transactions_waiting_for
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE status = 'PENDING_REFERENCE';

CREATE INDEX wager_transactions_wallet ON wager_transactions (wallet_id, created_at);

-- Estado terminal é terminal, e identidade não muda nunca.
CREATE FUNCTION wager_transactions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'transação % não pode ser removida', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'transação % está em estado terminal (%)', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.id, NEW.origin, NEW.kind, NEW.wallet_id, NEW.player_id,
        NEW.amount_minor, NEW.currency, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.origin, OLD.kind, OLD.wallet_id, OLD.player_id,
        OLD.amount_minor, OLD.currency, OLD.created_at)
       OR (NEW.provider_id, NEW.external_transaction_id, NEW.idempotency_key,
           NEW.payload_hash, NEW.round_id, NEW.game_id,
           NEW.reference_external_transaction_id)
       IS DISTINCT FROM
          (OLD.provider_id, OLD.external_transaction_id, OLD.idempotency_key,
           OLD.payload_hash, OLD.round_id, OLD.game_id,
           OLD.reference_external_transaction_id) THEN
        RAISE EXCEPTION 'colunas imutáveis da transação % não podem mudar', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER wager_transactions_guard BEFORE UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();

-- ─── ledger ─────────────────────────────────────────────────────────────────

-- Append-only. `seq` dá a ordem estável que a paginação por cursor precisa.
CREATE TABLE ledger_entries (
    seq                  BIGINT      GENERATED ALWAYS AS IDENTITY UNIQUE,
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL REFERENCES wallets (id),
    transaction_id       UUID        NOT NULL,
    direction            TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor         BIGINT      NOT NULL CHECK (amount_minor > 0),
    currency             CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_before_minor BIGINT      NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT      NOT NULL CHECK (balance_after_minor >= 0),
    created_at           TIMESTAMPTZ NOT NULL,

    -- Unicidade exigida pelo enunciado.
    CONSTRAINT ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),

    -- FK composta: o lançamento não pode apontar para uma transação de outra
    -- carteira, de outra moeda ou de outro valor. Junto com a unicidade acima,
    -- isso dá "no máximo um lançamento por transação" sem índice redundante,
    -- porque uma transação tem exatamente uma carteira.
    CONSTRAINT ledger_entries_transaction_fkey
        FOREIGN KEY (transaction_id, wallet_id, currency, amount_minor)
        REFERENCES wager_transactions (id, wallet_id, currency, amount_minor),

    -- A aritmética do lançamento, verificada pelo banco.
    CONSTRAINT ledger_entries_balance_math CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR
        (direction = 'DEBIT'  AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

CREATE INDEX ledger_entries_wallet_seq ON ledger_entries (wallet_id, seq);

-- Imutabilidade: o enunciado exige que o append-only seja imposto pelos
-- mecanismos do banco. Nenhuma constraint impede UPDATE — só trigger.
CREATE FUNCTION ledger_entries_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger é append-only (% recusado)', TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER ledger_entries_immutable BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_immutable();

CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_immutable();

-- Encadeamento: o saldo anterior de um lançamento é o saldo posterior do
-- lançamento anterior da mesma carteira. É o que impede um buraco na história.
CREATE FUNCTION ledger_entries_chain() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    anterior        BIGINT;
    moeda_carteira  CHAR(3);
BEGIN
    SELECT currency INTO moeda_carteira FROM wallets WHERE id = NEW.wallet_id;
    IF moeda_carteira IS DISTINCT FROM NEW.currency THEN
        RAISE EXCEPTION 'moeda do lançamento (%) difere da moeda da carteira (%)',
            NEW.currency, moeda_carteira USING ERRCODE = 'check_violation';
    END IF;

    SELECT balance_after_minor INTO anterior FROM ledger_entries
     WHERE wallet_id = NEW.wallet_id ORDER BY seq DESC LIMIT 1;

    IF COALESCE(anterior, 0) <> NEW.balance_before_minor THEN
        RAISE EXCEPTION 'lançamento da carteira % começa em % mas o ledger está em %',
            NEW.wallet_id, NEW.balance_before_minor, COALESCE(anterior, 0)
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER ledger_entries_chain BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_chain();

-- ─── saldo e ledger casam no commit ─────────────────────────────────────────
--
-- Constraint triggers ADIADOS. Disparam nos dois lados: alterar a carteira sem
-- lançar no ledger falha, e lançar no ledger sem atualizar a carteira também.
-- É esta dupla que transforma "cada mudança financeira exige o lançamento
-- correspondente, confirmado junto com o saldo" numa garantia do banco.
--
-- Precisa ser DEFERRED porque dentro da transação os dois passos acontecem em
-- momentos distintos; só no commit é que o par tem de estar coerente.

CREATE FUNCTION wallet_matches_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    guardado  BIGINT;
    ultimo    BIGINT;
    carteira  UUID;
BEGIN
    -- IF em vez de CASE: o plpgsql resolve os dois ramos de uma expressão
    -- CASE, e NEW.wallet_id não existe no registro de `wallets`. Com IF, só o
    -- ramo executado é compilado.
    IF TG_TABLE_NAME = 'wallets' THEN
        carteira := NEW.id;
    ELSE
        carteira := NEW.wallet_id;
    END IF;

    SELECT balance_minor INTO guardado FROM wallets WHERE id = carteira;
    SELECT balance_after_minor INTO ultimo FROM ledger_entries
     WHERE wallet_id = carteira ORDER BY seq DESC LIMIT 1;

    IF COALESCE(ultimo, 0) <> guardado THEN
        RAISE EXCEPTION 'saldo da carteira % é % mas o ledger termina em %',
            carteira, guardado, COALESCE(ultimo, 0) USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER wallets_match_ledger
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_matches_ledger();

CREATE CONSTRAINT TRIGGER ledger_entries_match_wallet
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_matches_ledger();

-- ─── inbox ──────────────────────────────────────────────────────────────────

-- Deduplicação durável do consumidor. A janela de dedupe do SQS FIFO é de
-- 5 minutos; uma reentrega depois disso passa direto. Esta tabela é a
-- garantia, e o FIFO é só otimização.
CREATE TABLE inbox_messages (
    consumer_name  TEXT        NOT NULL CHECK (consumer_name <> ''),
    message_id     TEXT        NOT NULL CHECK (message_id <> ''),
    payload_hash   TEXT        NOT NULL,
    transaction_id UUID        REFERENCES wager_transactions (id),
    received_at    TIMESTAMPTZ NOT NULL,
    processed_at   TIMESTAMPTZ,
    PRIMARY KEY (consumer_name, message_id)
);

-- ─── outbox ─────────────────────────────────────────────────────────────────

-- `payload` é JSON e não JSONB de propósito: JSONB normaliza chaves e espaços,
-- e o enunciado pede um snapshot imutável. JSON preserva o texto exato.
CREATE TABLE outbox_events (
    seq              BIGINT      GENERATED ALWAYS AS IDENTITY UNIQUE,
    event_id         UUID        PRIMARY KEY,
    event_type       TEXT        NOT NULL CHECK (event_type <> ''),
    event_version    INTEGER     NOT NULL CHECK (event_version >= 1),
    aggregate_type   TEXT        NOT NULL CHECK (aggregate_type <> ''),
    aggregate_id     UUID        NOT NULL,
    partition_key    TEXT        NOT NULL CHECK (partition_key <> ''),
    payload          JSON        NOT NULL,
    correlation_id   TEXT        NOT NULL DEFAULT '',
    causation_id     TEXT,
    occurred_at      TIMESTAMPTZ NOT NULL,
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  TIMESTAMPTZ NOT NULL,
    published_at     TIMESTAMPTZ,
    last_error       TEXT,
    dead_lettered_at TIMESTAMPTZ,
    CONSTRAINT outbox_events_single_outcome CHECK (published_at IS NULL OR dead_lettered_at IS NULL)
);

-- Índice que serve ao SELECT ... FOR UPDATE SKIP LOCKED do publisher.
CREATE INDEX outbox_events_unpublished ON outbox_events (next_attempt_at, seq)
    WHERE published_at IS NULL AND dead_lettered_at IS NULL;

CREATE FUNCTION outbox_events_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evento % não pode ser removido', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.event_id, NEW.event_type, NEW.event_version, NEW.aggregate_type,
        NEW.aggregate_id, NEW.partition_key, NEW.payload::text, NEW.occurred_at)
       IS DISTINCT FROM
       (OLD.event_id, OLD.event_type, OLD.event_version, OLD.aggregate_type,
        OLD.aggregate_id, OLD.partition_key, OLD.payload::text, OLD.occurred_at) THEN
        RAISE EXCEPTION 'snapshot do evento % é imutável', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'evento % já foi publicado', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER outbox_events_guard BEFORE UPDATE OR DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard();

-- ─── papel de execução (menor privilégio) ───────────────────────────────────
--
-- As migrations rodam como dono do schema. A aplicação conecta com um login
-- membro deste grupo, que NÃO tem UPDATE, DELETE nem TRUNCATE no ledger.
-- Sem ser dono nem superusuário, o runtime também não consegue desabilitar
-- trigger nem mudar session_replication_role — ou seja, o append-only resiste
-- até a um bug da própria aplicação.
DO $$
BEGIN
    CREATE ROLE wallet_app NOLOGIN;
EXCEPTION WHEN duplicate_object OR unique_violation THEN
    NULL;  -- papéis são do cluster inteiro; reaproveita se já existir
END $$;

GRANT USAGE ON SCHEMA public TO wallet_app;
GRANT SELECT, INSERT ON ledger_entries TO wallet_app;
GRANT SELECT, INSERT, UPDATE ON wallets, wager_transactions, inbox_messages, outbox_events TO wallet_app;

-- O login da aplicação entra no grupo aqui, e não no script de init do
-- Postgres: lá o papel wallet_app ainda não existe, porque é esta migration
-- que o cria. Se o login ainda não tiver sido criado (execução fora do
-- compose), a associação é simplesmente pulada.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wallet_service') THEN
        GRANT wallet_app TO wallet_service;
    END IF;
END $$;
