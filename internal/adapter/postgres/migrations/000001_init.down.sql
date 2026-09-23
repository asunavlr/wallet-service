-- Reversão completa da migration inicial.
-- A ordem importa: tabelas antes das funções que as triggers usam.
DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS inbox_messages;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;

DROP FUNCTION IF EXISTS outbox_events_guard();
DROP FUNCTION IF EXISTS wallet_matches_ledger();
DROP FUNCTION IF EXISTS ledger_entries_chain();
DROP FUNCTION IF EXISTS ledger_entries_immutable();
DROP FUNCTION IF EXISTS wager_transactions_guard();
DROP FUNCTION IF EXISTS wallets_guard();

-- O papel não é removido: pode ser compartilhado com outros bancos do cluster
-- e pode ter objetos dependentes. Remover manualmente com DROP ROLE wallet_app.
