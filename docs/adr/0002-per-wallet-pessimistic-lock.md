---
status: accepted
---

# Lock pessimista por carteira (`FOR NO KEY UPDATE`) com coerência saldo/ledger imposta pelo banco

Toda operação financeira trava a linha da carteira com `SELECT ... FOR NO KEY UPDATE` dentro da database transaction, e o `UPDATE` do saldo é condicionado pela `version`. Além do `CHECK (balance_minor >= 0)`, triggers diferidos garantem no commit que toda mudança de saldo tem o lançamento correspondente no ledger, com `balance_before` e `balance_after` encadeados. A ordem de aquisição é sempre: leitura rápida de idempotência, lock da carteira, claim da idempotência, resolução da Reference.

## Considered Options

- **Controle otimista com retry limitado:** sob disputa na mesma carteira (o teste obrigatório das duas apostas de 80 em 100) vira uma sequência de conflitos e novas tentativas, e o resultado depende de temporização.
- **Isolamento `SERIALIZABLE`:** abortos em massa sob contenção, sem controle sobre quem perde.
- **Atualização atômica condicionada** (`SET balance = balance - x WHERE balance >= x`) sem lock explícito: resolve o saldo, mas não serializa a resolução de Reference nem a ordem entre idempotência, ledger e outbox.

## Consequences

- `FOR NO KEY UPDATE`, e não `FOR UPDATE`: os `INSERT` com chave estrangeira para `wallets` tomam `FOR KEY SHARE` na linha, que conflita com `FOR UPDATE` e causaria deadlock entre duas operações da mesma carteira.
- Operações da mesma carteira são serializadas; carteiras diferentes não se bloqueiam e avançam em paralelo. Não há lock global.
- Os triggers de coerência acrescentam duas buscas por índice ao commit de cada operação financeira, e impõem uma operação financeira por database transaction.
