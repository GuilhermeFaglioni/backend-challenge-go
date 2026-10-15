# Wagering

Serviço que processa operações financeiras de provedores de jogos (apostas, prêmios, estornos) sobre as carteiras dos jogadores, com garantias de saldo, idempotência e rastreabilidade em várias instâncias.

## Language

Os termos canônicos estão em inglês, como no código. As descrições estão em português, como a documentação. "Transaction" nunca aparece sozinho: a transação do banco de dados é sempre *database transaction*.

**Wager Transaction**:
O registro persistido de uma Operation request aceita, ou da abertura interna de uma carteira, com tipo, valor, estado e resultado. É o que a idempotência e o replay consultam.
_Avoid_: transaction (sem qualificar), operação (como nome do registro)

**Operation request**:
O pedido que um provedor envia, por HTTP ou SQS, para movimentar uma carteira, com tipo, valor e identificadores. Ao ser aceito, vira uma Wager Transaction.
_Avoid_: transaction, operation (sozinho), payload, mensagem

**Ledger Entry**:
O lançamento imutável que registra uma mudança de saldo de uma carteira e aponta para a Wager Transaction que a causou. Só existe quando o saldo muda.
_Avoid_: transação do ledger, movimento

**Reference**:
A Wager Transaction apontada por outra, resolvida por `(provedor, id externo)`. É a única palavra para isso, seja a apontada por uma Reversal ou por um `WIN`.
_Avoid_: original, transação original

**Reversal**:
Um `REFUND` ou um `ROLLBACK`: uma Wager Transaction cujo efeito é desfazer o de uma Reference. Um `WIN` que aponta uma `BET` tem Reference, mas não é Reversal.
_Avoid_: estorno (ambíguo entre os dois tipos)

**Pending Reference**:
O estado de uma Wager Transaction que espera a sua Reference chegar (`PENDING_REFERENCE`).
_Avoid_: pendente (sem qualificar)

**Rejection**:
O fim de uma Wager Transaction em `REJECTED` por regra de negócio (saldo insuficiente, Reference inválida, moeda errada). É um resultado definitivo e válido; carrega um `failureCode`, nome do campo que vem do contrato do enunciado.
_Avoid_: falha (sem qualificar), erro de negócio

**Failure**:
O fim de uma Wager Transaction em `FAILED` por defeito permanente de infraestrutura, registrado para auditoria (`INTERNAL_PERMANENT_FAILURE`).
_Avoid_: falha (sem qualificar), erro permanente

**Transient error**:
Um erro temporário (Postgres ou SQS indisponível, deadlock, timeout) que desfaz a database transaction e é repetido. Não vira estado nem `failureCode`.
_Avoid_: falha temporária, falha (sem qualificar)
