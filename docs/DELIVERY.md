# Entrega

Este documento liga cada exigência do enunciado ([CHALLENGE.md](CHALLENGE.md)) à sua evidência: um teste que se pode rodar, um documento ou um comando. Foi escrito para quem nunca viu o projeto. As decisões e as justificativas estão em [ARCHITECTURE.md](../ARCHITECTURE.md); como rodar, no [README.md](../README.md).

## Do checkout limpo à API em três comandos

```bash
docker compose up --build
```

```bash
bash .github/scripts/smoke.sh
```

```bash
go test ./... && go vet ./...
```

O primeiro sobe PostgreSQL (com as migrations), LocalStack (as filas e a DLQ com redrive), Keycloak (o realm importado, com as identidades de teste) e três instâncias da aplicação. O segundo é a verificação que o CI também roda contra o Compose: migrations, instâncias prontas, tokens, filas, uma carteira, uma aposta repetida em outra instância, um `WIN`, um `LOSS`, um `REFUND`, uma reversão que chega antes da aposta (e é aplicada por um worker), uma mensagem SQS aplicada e uma inválida que vai para a DLQ, e a reconciliação. Os exemplos de chamada de cada endpoint estão no README.

## Critérios de avaliação

| Critério (pontos) | Onde está a evidência |
| --- | --- |
| **Integridade financeira (20)** | `Money` em `int64` de unidades menores, sem ponto flutuante (ADR [0003](adr/0003-money-as-int64-minor-units.md), `internal/domain/money`, testes de parsing, escala, overflow e moedas). O banco recusa por SQL direto o que a aplicação nunca faria: saldo sem lançamento, lançamento órfão ou que não encadeia, saldo negativo, `UPDATE`/`DELETE`/`TRUNCATE` no ledger, lançamento que não combina com o tipo da transação, segunda reversão da mesma referência (`test/integration/schema_test.go`). Reversões e a regra de cada código de recusa: `internal/domain/wager/reversal_test.go`, `test/integration/reversal_test.go`. Reconciliação em snapshot consistente: `test/integration/reconciliation_test.go` (divergência forçada, concorrente com escritas, sem alterar nada). Todo cenário de integração e de multi-instância termina conferindo o saldo contra créditos menos débitos. |
| **Concorrência (20)** | Lock pessimista por carteira (ADR [0002](adr/0002-per-wallet-pessimistic-lock.md)) e ordem de aquisição documentada (ARCHITECTURE §7). Provado entre processos reais: `test/multiinstance/guarantees_test.go` (mesma aposta 50 vezes, duas de 80 sobre 100, carteiras distintas sem espera entre si, operações mistas, `REFUND` e `ROLLBACK` disputando a mesma aposta). Uma corrida real foi achada por essa suíte e corrigida (ARCHITECTURE §8). |
| **Idempotência (15)** | Persistente, escopada por provedor, com hash canônico igual para HTTP e SQS, replay com o saldo original, conflito (`409`) e id externo duplicado: ARCHITECTURE §8; `internal/application/processwager`, `test/integration/processwager_test.go`, `wagering_api_test.go`; entre HTTP e SQS: `test/integration/consumer_test.go`. Sobrevive ao reinício de todos os processos: `TestAfterRestartingEveryInstance...`. |
| **Mensageria e recuperação (15)** | Inbox na mesma transação, `DeleteMessage` só depois do commit, DLQ com motivo, backoff por recebimento, ordem por grupo, `SIGTERM`: ARCHITECTURE §12 e §14; `internal/adapters/sqsconsumer`, `test/integration/consumer_test.go`, `test/multiinstance/consumer_test.go`. Outbox transacional com lease e `SKIP LOCKED`: `internal/adapters/outbox`, `test/multiinstance/outbox_test.go`. Referência pendente com worker, lease, backoff e expiração: `test/integration/pending_reference_test.go`, `test/multiinstance/pending_test.go`. |
| **Modelagem e arquitetura (10)** | Acesso à mensageria por componente, com políticas de privilégio mínimo como código (`deploy/aws/iam`) conferidas contra as chamadas do código (`test/iam`) e provisionadas no LocalStack (que não as aplica). Domínio sem dependência de Fx, HTTP, SQS ou persistência (teste de estrutura em `internal/domain/*/structure_test.go`), tipos imutáveis com construtores que validam, portas em `internal/application`, adapters em `internal/adapters`, composição por módulos Fx (`internal/app`). Autenticação RS256 local e autorização por papel declarada em cada rota, com um teste que reprova rota sem política (`internal/adapters/httpapi/routes_test.go`). |
| **Testes (10)** | Camadas e comandos abaixo. Integração com PostgreSQL, Keycloak e LocalStack reais (nada disso é trocado por mock); isolamento entre provedores (`transactions_api_test.go`); paralelismo e interrupção com processos reais e pontos de falha. Cada garantia dos PRs foi vista falhando sem o seu teste (mutações descritas em cada PR). |
| **Observabilidade (5)** | Logs JSON com `correlationId`, `messageId`, `transactionId`, `walletId` e `providerId`, sem payload, token nem chave (testes "nothing is logged"); métricas por desfecho, duplicata, retry, DLQ, conflito de concorrência, atraso da outbox, latência, divergência de reconciliação e referências pendentes (ARCHITECTURE §15); `/health/live` e `/health/ready` (que falha durante o shutdown). |
| **Documentação (5)** | README (pré-requisitos, variáveis, filas, migrations e seu rollback, exemplos, comandos de teste), `.env.example`, este documento, ARCHITECTURE.md, OpenAPI servido pela própria aplicação e Swagger UI no Compose, ADRs e CONTEXT.md. |

## Eliminatórios

| Eliminatório | Como é evitado | Prova |
| --- | --- | --- |
| Sem autenticação efetiva nos endpoints de negócio | Toda rota de negócio é registrada com papéis; token RS256 validado localmente (emissor, audiência, expiração) | `routes_test.go`; `test/integration/auth_test.go`; os testes de "negado não cria nada" |
| Acesso não autorizado a operações ou transações | Escopo do provedor no `WHERE` da leitura; `providerId` do corpo igual à claim; `404` indistinguível para o que não é seu | `transactions_api_test.go`, `wagering_api_test.go` |
| Ponto flutuante em dinheiro | `int64` de unidades menores; entrada exata em string | testes de `Money` |
| Saldo negativo por concorrência | `CHECK` no banco, lock por carteira, `UPDATE` condicionado à versão | `schema_test.go`; guarantees (duas de 80) |
| Movimentação duplicada | Unicidade de chave, id externo e lançamento por transação; índice de uma reversão por referência | guarantees; `schema_test.go` |
| Idempotência só em memória | Tabela `wager_transactions` + inbox | reinício de todos os processos |
| Dependência de uma única instância | Nenhum estado em memória; leases e `SKIP LOCKED`; três processos nos testes | todo `test/multiinstance` |
| Publicação anterior ao commit | Eventos só entram na outbox, na mesma transação | `test/integration/outbox_*`, `uow_test.go` |
| Sem ledger auditável | Ledger append-only (trigger e privilégios), com `seq` e encadeamento | `schema_test.go`; `ledger_api_test.go` |
| PostgreSQL, SQS e IdP substituídos por mocks | Os três são reais nos testes de integração e multi-instância | `test/infra` |

## Os oito cenários obrigatórios

Executados de ponta a ponta contra três processos reais da aplicação, com o detector de corrida, em 15/10/2026 às 19:49 UTC (macOS arm64, Docker Desktop, Go 1.27.2), com `go test -tags multiinstance -race -count=1 -shuffle=on -v ./test/multiinstance/`: 24 testes, todos passaram, em 3m12s.

| # | Cenário do enunciado | Teste | Resultado |
| ---: | --- | --- | --- |
| 1 | A mesma aposta 50 vezes em paralelo, um único débito | `TestTheSameBetSentFiftyTimesInParallelIsDebitedOnce` | passou (4,2s) |
| 2 | Duas apostas de 80,00 sobre 100,00 | `TestTwoBetsOf80On100AtOnceOnlyOneIsProcessed` | passou (6,7s) |
| 3 | Carteiras distintas simultaneamente | `TestOperationsOnDifferentWalletsDoNotWaitForEachOther` | passou (4,3s) |
| 4 | Três instâncias independentes | todos os testes da camada; `TestMixedOperationsAcrossThreeProcessesAddUpOnce`, `TestThreeInstancesCompetingNeverHandleAMessageTwice` | passaram (4,5s, 5,1s) |
| 5 | Consumidor interrompido depois do commit e antes da remoção | `TestACrashAfterTheCommitAndBeforeTheDeleteChangesNothingWhenTheMessageComesBack`; também `TestAConsumerKilledMidFlightIsReplacedByAnother` e `TestATerminatedConsumerFinishesWhatItHasAndTheOthersTakeTheRest` | passaram (7,8s, 12,0s, 13,0s) |
| 6 | Dois publishers disputando a outbox; recuperação da publicação | `TestPublishersOnSeveralInstancesNeverLoseAnEvent`, `TestAPublisherKilledAfterReservingLeavesTheEventsToAnotherInstance`, `TestAPublisherKilledBetweenPublishingAndMarkingLeavesTheEventsToAnotherInstance` | passaram (4,4s, 4,8s, 4,8s) |
| 7 | `REFUND`/`ROLLBACK` antes da referência: resolução posterior ou rejeição por expiração | `TestAReversalBeforeItsBetIsAppliedOnceAcrossProcesses`, `TestAReferenceThatNeverComesIsRefusedWhenTheWaitRunsOut`, `TestThreeWorkersNeverApplyAWaitingOperationTwice`; no banco real e na fila: `test/integration/pending_reference_test.go` | passaram (3,9s, 11,0s, 5,6s) |
| 8 | Reinício preserva idempotência, pendências e consistência; processo interrompido depois de aceitar a espera e antes de executar, outra instância retoma | `TestAfterRestartingEveryInstanceEverythingReplaysAndTheBalanceIsUnchanged`, `TestAWaitSurvivesTheRestartOfEveryProcess`, `TestAWaitCommittedByAProcessThatThenDiesIsResolvedByAnother`, `TestAWorkerKilledHoldingAnOperationIsReplacedByAnotherProcess` | passaram (7,6s, 5,0s, 2,9s, 5,6s) |

Cada cenário termina conferindo o saldo armazenado contra créditos menos débitos do ledger, e pedindo a **outro processo**, pelo endpoint de reconciliação, que diga o mesmo (`books()` em `guarantees_test.go`). Os cenários que cruzam HTTP e SQS para a mesma operação estão em `test/integration/consumer_test.go`, `win_loss_test.go` e `reversal_test.go`, e os testes de duplicidade enviam cada cópia com um `MessageDeduplicationId` diferente, para que a deduplicação do FIFO não faça o trabalho da aplicação.

## Camadas de teste e comandos

| Camada | Comando | Tamanho |
| --- | --- | ---: |
| Unitários | `go test -race ./...` | 471 testes; domínio com 96,9% a 98,2% de cobertura só nesta camada |
| Integração (Docker) | `go test -tags integration -race -count=1 -shuffle=on ./test/...` | 174 testes, cerca de 2m20s |
| Multi-instância e falhas (Docker) | `go test -tags multiinstance -race -count=1 -shuffle=on ./test/multiinstance/` | 24 testes, 3m12s |
| Qualidade | `gofmt -l .`, `go vet ./...` (e com cada tag), `golangci-lint run ./...` | sem achados |
| Ambiente completo | `bash .github/scripts/smoke.sh` | o mesmo do CI |
| Carga (opcional, Compose de pé) | `make loadtest` | ver [LOAD_TEST.md](LOAD_TEST.md) |

O CI roda tudo isso, mais a cobertura combinada das três camadas (gate de 90% por pacote do domínio), vulnerabilidades e a construção da imagem multi-arquitetura.

## Limitações e o que não foi feito

O texto completo está em ARCHITECTURE §17. Em resumo:

- O teste de carga (opcional) foi feito e está em [LOAD_TEST.md](LOAD_TEST.md), com a ressalva de que mede ordens de grandeza num notebook e de que uma carteira só é publicada na outbox a ~150 eventos/s. Ficaram de fora partidas dobradas, tracing e dashboards.
- A inbox e a outbox não têm política de retenção, e a outbox publica no máximo ~150 eventos/s por carteira.
- A reconciliação soma o ledger inteiro; o custo cresce com o número de lançamentos.
- Os tokens locais usam `http`. (`/metrics` exige o papel `monitoring`, do cliente `metrics-scraper`.)

## Release

Proposta de tag para quem mantém o repositório, depois de confirmar o CI verde na `main`:

```bash
git tag -a v1.0.0 -m "Wagering service: first complete delivery"
git push origin v1.0.0
```

A tag dispara a publicação da imagem com as tags `1.0.0` e `1.0` (o workflow e a proteção das tags `v*` estão descritos em ARCHITECTURE §18).

> **Lembrete:** o pacote da imagem no GitHub Container Registry nasce **privado**. Para que `docker pull ghcr.io/guilhermefaglioni/backend-challenge-go:latest` funcione para quem avalia, o dono do repositório precisa torná-lo público uma vez (GitHub, aba Packages, configurações do pacote, visibilidade). Sem isso, o caminho de `docker compose up --build` continua funcionando, pois compila a imagem localmente.
