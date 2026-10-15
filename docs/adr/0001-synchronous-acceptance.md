---
status: accepted
---

# Aceite síncrono: a resposta só sai depois do commit do resultado

`POST /wagering/transactions` e o consumidor SQS executam a operação inteira dentro de uma única database transaction (claim de idempotência, lock da carteira, saldo, ledger, outbox) e só respondem depois do commit. O estado `PENDING` existe apenas dentro dessa transação; o único estado intermediário que chega a ser persistido é `PENDING_REFERENCE`.

## Considered Options

- **Aceite assíncrono:** commit de um `PENDING` e resposta `202`, com um worker que executa depois. Rejeitado: cria uma janela em que um registro confirmado e não executado pode ficar órfão se a instância morrer, o que obriga a um worker de retomada com lease, mais testes de recuperação, e o provedor passa a precisar consultar o resultado depois.

## Consequences

- Uma instância que cai antes do commit não deixa registro nenhum; o provedor reenvia com a mesma chave e a idempotência cuida do resto.
- A latência do provedor inclui a espera pelo lock da carteira. Sob contenção alta na mesma carteira, as requisições esperam em fila no banco em vez de receberem `202`.
- O worker de Pending Reference também retoma qualquer linha `PENDING` antiga, só por defesa; no desenho normal nenhuma existe.
