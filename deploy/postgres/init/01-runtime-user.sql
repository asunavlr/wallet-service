-- Usuário de execução do serviço, criado só no ambiente local.
--
-- Ele é membro de wallet_app, o papel NOLOGIN que a migration cria com
-- privilégio mínimo: sem UPDATE, DELETE ou TRUNCATE no ledger. Sem ser dono
-- nem superusuário, também não consegue desabilitar trigger nem mudar
-- session_replication_role — o append-only resiste até a um bug da aplicação.
--
-- Em produção a senha vem de um cofre de segredos, nunca de um arquivo.
DO $$
BEGIN
    CREATE ROLE wallet_service LOGIN PASSWORD 'dev';
EXCEPTION WHEN duplicate_object THEN
    NULL;
END $$;
