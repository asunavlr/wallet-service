#!/usr/bin/env bash
# Confere a solução contra o enunciado, item a item.
#
# Cada linha é uma EXIGÊNCIA do desafio seguida da evidência que a sustenta —
# um teste que roda, uma constraint que existe, uma rota que recusa. Nada aqui
# é afirmação: tudo é comando executado na hora.
#
#   ./scripts/verificar.sh          precisa do `docker compose up -d` antes
set -uo pipefail
cd "$(dirname "$0")/.."

ok=0; falhou=0
v()  { printf '  \033[32m✓\033[0m %s\n' "$1"; ok=$((ok+1)); }
x()  { printf '  \033[31m✗\033[0m %s\n' "$1"; falhou=$((falhou+1)); }
sec(){ printf '\n\033[1m%s\033[0m\n' "$1"; }

# checa(rótulo, comando): o comando precisa sair com 0
checa() { local r="$1"; shift; if "$@" >/dev/null 2>&1; then v "$r"; else x "$r"; fi; }

# As três suítes rodam UMA vez cada, e as checagens consultam a saída
# capturada. Rodar `go test -run` por exigência recompilava o projeto inteiro
# a cada linha: trinta vezes o mesmo trabalho, e a conferência ficava mais
# lenta que a suíte que ela confere.
RESULTADOS=$(mktemp)
trap 'rm -f "$RESULTADOS"' EXIT

printf 'Rodando as três suítes (leva cerca de dois minutos)...\n'
go test -count=1 ./... -v                                  2>&1 >>"$RESULTADOS" || true
go test -count=1 -tags integration ./test/integration/ -v  2>&1 >>"$RESULTADOS" || true
go test -count=1 -tags e2e ./test/e2e/ -v -timeout 25m     2>&1 >>"$RESULTADOS" || true

# teste(rótulo, nome): aquele teste precisa constar como PASS na saída.
# Ausente conta como falha — um teste que não rodou não prova nada.
teste() {
  local r="$1" nome="$2"
  if grep -q -- "--- PASS: ${nome}\b" "$RESULTADOS"; then
    v "$r  ($nome)"
  elif grep -q -- "--- \(FAIL\|SKIP\): ${nome}\b" "$RESULTADOS"; then
    x "$r  ($nome falhou ou foi pulado)"
  else
    x "$r  ($nome não foi executado)"
  fi
}
# sql(rótulo, consulta, esperado)
sql() {
  local r="$1" q="$2" esperado="$3" got
  got=$(docker compose exec -T postgres psql -U wallet -d wallet -tAc "$q" 2>/dev/null | tr -d '[:space:]')
  if [ "$got" = "$esperado" ]; then v "$r"; else x "$r (veio '$got', queria '$esperado')"; fi
}

sec "QUALIDADE DE CÓDIGO"
checa "go vet sem avisos"            go vet ./...
checa "gofmt: tudo formatado"        bash -c '[ -z "$(gofmt -l .)" ]'
checa "go build"                     go build ./...
checa "dependências reproduzíveis (go.sum versionado)" test -f go.sum

sec "ELIMINATÓRIOS"
teste "dinheiro nunca passa por float (varredura da AST)" "TestNenhumFloatNoCaminhoDoDinheiro"
teste "idempotência sobrevive ao reinício dos processos"  "TestIdempotenciaSobreviveAoReinicio"
teste "sem saldo negativo por concorrência"               "TestDuasApostasDisputandoOMesmoSaldo"
teste "sem movimentação duplicada"                        "TestCinquentaEnviosDaMesmaApostaDebitamUmaVez"
teste "evento só é publicado depois do commit"            "TestEventoSoApareceDepoisDoCommit"
teste "não depende de uma instância única"                "TestDoisPublishersNaoDuplicamTrabalho"
teste "rotas de negócio exigem autenticação"              "TestRotasDeNegocioExigemCredencial"
teste "nenhum acesso não autorizado a operações"          "TestIsolamentoEntreProvedores"
teste "provedor não navega em carteira alheia"            "TestProvedorNaoNavegaEmCarteira"
teste "infra real nos testes (Postgres, SQS, IdP)"        "TestTresInstanciasNoAr"

sec "INVARIANTES IMPOSTAS PELO BANCO"
# A checagem nomeia a constraint em vez de contar por padrão: contar
# aproximado acusa falha quando o schema ganha outra regra de saldo, que é
# justamente o contrário do que se quer.
sql "saldo da carteira não pode ser negativo" "select count(*) from pg_constraint c join pg_class t on t.oid=c.conrelid where t.relname='wallets' and c.contype='c' and pg_get_constraintdef(c.oid) like '%balance_minor >= 0%'" "1"
sql "lançamento não pode ter saldo negativo"  "select count(*) from pg_constraint c join pg_class t on t.oid=c.conrelid where t.relname='ledger_entries' and c.contype='c' and pg_get_constraintdef(c.oid) like '%balance_%_minor >= 0%'" "2"
sql "aritmética do lançamento é CHECK"        "select count(*) from pg_constraint where conname='ledger_entries_balance_math'" "1"
sql "ledger é append-only (triggers)" "select count(*) from pg_trigger where tgname like 'ledger_entries_immutable%' or tgname like 'ledger_entries_no_truncate%'" "2"
sql "saldo e ledger casam no commit"  "select count(*) from pg_trigger where tgname in ('wallets_match_ledger','ledger_entries_match_wallet')" "2"
sql "uma reversão por transação"      "select count(*) from pg_indexes where indexname='wager_transactions_single_reversal'" "1"
sql "operação externa única"          "select count(*) from pg_indexes where indexname='wager_transactions_external_key'" "1"
sql "chave de idempotência única"     "select count(*) from pg_indexes where indexname='wager_transactions_idempotency_key'" "1"
sql "uma abertura por carteira"       "select count(*) from pg_indexes where indexname='wager_transactions_opening_key'" "1"
sql "runtime sem UPDATE no ledger"    "select count(*) from information_schema.role_table_grants where grantee='wallet_app' and table_name='ledger_entries' and privilege_type in ('UPDATE','DELETE')" "0"

sec "INTEGRIDADE DOS DADOS AGORA"
sql "nenhum saldo negativo"           "select count(*) from wallets where balance_minor < 0" "0"
sql "nenhum lançamento órfão"         "select count(*) from ledger_entries l where not exists (select 1 from wager_transactions t where t.id=l.transaction_id)" "0"
sql "todo saldo bate com o ledger"    "select count(*) from wallets w where w.balance_minor <> coalesce((select sum(case when direction='CREDIT' then amount_minor else -amount_minor end) from ledger_entries where wallet_id=w.id),0)" "0"
sql "nenhum evento na dead-letter"    "select count(*) from outbox_events where dead_lettered_at is not null" "0"
sql "nenhuma transação em PENDING"    "select count(*) from wager_transactions where status='PENDING'" "0"

sec "OS 8 CENÁRIOS OBRIGATÓRIOS"
teste "1 · mesma aposta 50x em paralelo"          "TestCinquentaEnviosDaMesmaApostaDebitamUmaVez"
teste "2 · 100.00 com duas apostas de 80.00"      "TestDuasApostasDisputandoOMesmoSaldo"
teste "3 · carteiras distintas em paralelo"       "TestCarteirasDistintasAvancamEmParalelo"
teste "4 · três instâncias independentes"         "TestDuasApostasEmInstanciasDiferentes"
teste "5 · consumidor morto antes de remover"     "TestConsumidorInterrompidoAntesDeRemoverAMensagem"
teste "6 · dois publishers na mesma outbox"       "TestDoisPublishersNaoDuplicamTrabalho"
teste "7 · reversão antes da referência"          "TestReversaoChegandoAntesDaReferencia"
teste "8 · reinício completo da aplicação"        "TestEstadoSobreviveAoReinicioCompleto"

sec "ALÉM DO EXIGIDO"
teste "composição Fx inicia e encerra"            "TestComposicaoFxIniciaEEncerra"
teste "SIGTERM com trabalho em voo"               "TestSigtermComTrabalhoEmVoo"
teste "PostgreSQL caindo no meio do tráfego"      "TestPostgresCaindoNoMeioDoTrafego"

sec "RESULTADO"
printf "  %d verificações OK, %d falharam\n" "$ok" "$falhou"
if [ "$falhou" -gt 0 ]; then
  printf "\n  \033[31mAlgo não confere. Rode com -v o teste que falhou.\033[0m\n\n"
  exit 1
fi
printf "\n  \033[32mTudo confere.\033[0m\n\n"
