# Processo de trabalho

Como o projeto foi planejado, dividido e verificado, e qual foi o papel de uma IA nele. Este documento descreve o processo; as decisões técnicas estão no [ARCHITECTURE.md](../ARCHITECTURE.md) e nos [ADRs](adr), e o estado do trabalho, nas issues do GitHub.

## Uso de IA

O código, os testes e a documentação foram escritos com o **Claude Code** (um assistente de IA da Anthropic) como implementador, sob direção do autor. A divisão foi esta:

| Quem | O quê |
| --- | --- |
| **Autor** | Escolher o que construir e em que ordem; decidir as interpretações do enunciado (ver "Interpretações" no ARCHITECTURE §17); aprovar o roadmap e cada fatia de ticket; pedir as análises contra o enunciado; decidir o que corrigir depois delas; autorizar cada merge |
| **IA** | Entrevistar o autor sobre cada decisão antes de escrever (uma pergunta por vez, com recomendação); implementar cada ticket; escrever os testes, inclusive os de mutação; manter a documentação; abrir os PRs; corrigir as falhas do CI |

A IA não decidiu sozinha o que entra no escopo. Quando a análise contra o enunciado achou lacunas, o autor escolheu corrigi-las.

## Do enunciado aos tickets

1. **Enunciado e desenho.** O ponto de partida é [CHALLENGE.md](CHALLENGE.md) e o ARCHITECTURE.md.
2. **Entrevista sobre as decisões.** Antes de escrever o CI/CD e o roadmap, o autor foi entrevistado ponto a ponto. Cada decisão difícil de reverter virou um ADR curto: [aceite síncrono](adr/0001-synchronous-acceptance.md), [lock pessimista por carteira](adr/0002-per-wallet-pessimistic-lock.md) e [dinheiro em `int64`](adr/0003-money-as-int64-minor-units.md). O vocabulário ficou em [CONTEXT.md](../CONTEXT.md).
3. **Roadmap em fatias verticais.** [ROADMAP.md](ROADMAP.md) tem 22 tickets (R1 a R22), com bloqueios entre eles. O raciocínio da ordem está lá: `Money` primeiro, autenticação junto do primeiro endpoint, a outbox cedo, `BET` completo antes dos outros tipos, o SQS logo depois.
4. **Publicação.** Cada ticket é uma issue com critérios de aceite e bloqueios nativos do GitHub. A divisão foi apresentada ao autor e aprovada antes de ser publicada. O CI/CD teve a própria sequência de tickets.

## Como cada ticket foi feito

Um branch por ticket, um PR por ticket, com a mesma rotina:

1. **Testes primeiro** (TDD), na menor fatia que atravessa domínio, schema, caso de uso, borda e testes.
2. **Teste de mutação manual.** Para cada garantia nova, a proteção é removida ou alterada e o teste precisa reprovar. A descrição do PR diz quais mutações foram feitas. É a definição de pronto nº 7 do roadmap.
3. **Suítes completas antes do PR:** lint, testes unitários com `-race`, integração (PostgreSQL, Keycloak e LocalStack reais) e multi-instância (três processos).
4. **CI verde** nos dez checks, com título em Conventional Commits. O autor autorizou o merge dos PRs com CI verde.
5. **Documentação junto do código:** ARCHITECTURE, README, OpenAPI e CONTEXT mudam no mesmo PR que muda o comportamento.

O resultado até a entrega: 33 issues, todas fechadas, e 42 PRs mesclados (de 8 de setembro a 15 de outubro de 2026).

## Como o resultado foi verificado

Duas vezes, depois dos 22 tickets, o resultado foi confrontado com o enunciado, requisito por requisito.

A primeira análise achou quatro lacunas reais, que viraram os PRs seguintes:

1. As credenciais e políticas do broker estavam descritas na documentação, mas não existiam: [#71](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/71).
2. `FAILED` nunca era produzido, e o worker repetia para sempre uma operação com defeito permanente: [#70](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/70).
3. A outbox publicava só ~20 eventos/s por carteira: [#72](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/72).
4. Limitações operacionais: sem retenção de inbox e outbox ([#73](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/73)), `/metrics` sem autenticação ([#74](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/74)) e reconciliação cujo custo crescia com o ledger ([#75](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/75), [#76](https://github.com/GuilhermeFaglioni/backend-challenge-go/pull/76)).

A segunda análise não encontrou requisito obrigatório pendente. A evidência de cada exigência, com o teste ou o comando que a prova, está em [DELIVERY.md](DELIVERY.md).

## Limites deste processo

- Os testes de mutação são **manuais**: um script edita o arquivo, roda o subconjunto de testes e restaura o arquivo. Não há uma ferramenta de mutação no CI, então a garantia vale para os PRs em que foi feita e está descrita neles.
- O LocalStack gratuito não aplica IAM; as políticas são verificadas contra o código, mas não barram chamadas localmente (ARCHITECTURE §13 e §17).
- O que ficou de fora (partidas dobradas, tracing, dashboards) e as limitações conhecidas estão no ARCHITECTURE §17.
