-- Banco separado para os testes de integração.
--
-- Sem ele, os testes compartilhariam o banco com as instâncias em execução —
-- e os relays de outbox delas publicariam os eventos que o teste acabara de
-- criar, antes que o teste pudesse observá-los. O sintoma é um teste que
-- falha em uma corrida a cada dez, e a causa não é do código: é do teste não
-- ser dono dos próprios dados.
CREATE DATABASE wallet_test OWNER wallet;
