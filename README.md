# Wagering service

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores, com várias instâncias em paralelo e tolerância a falhas.

- Enunciado do desafio: [docs/CHALLENGE.md](docs/CHALLENGE.md)
- Decisões técnicas: [ARCHITECTURE.md](ARCHITECTURE.md) e os registros curtos em [docs/adr](docs/adr)
- Ordem de construção e definição de pronto: [docs/ROADMAP.md](docs/ROADMAP.md)
- Entrega, evidência por critério e resultado dos cenários obrigatórios: [docs/DELIVERY.md](docs/DELIVERY.md); teste de carga: [docs/LOAD_TEST.md](docs/LOAD_TEST.md)
- Vocabulário do projeto: [CONTEXT.md](CONTEXT.md)

> **Status: entregue.** Todos os endpoints do enunciado, o consumidor SQS com inbox, a outbox, as operações `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK` (inclusive à espera da referência), a reconciliação, a autenticação e o CI/CD estão implementados e testados. O que foi medido, o que ficou de fora e a evidência de cada critério de avaliação estão em [docs/DELIVERY.md](docs/DELIVERY.md); as decisões, em [ARCHITECTURE.md](ARCHITECTURE.md).

## Pré-requisitos

- Go 1.27.2 (a mesma versão de `go.mod` e do `Dockerfile`)
- Docker com Docker Compose v2

## Executar

Sobe PostgreSQL, migrations, LocalStack (filas SQS), Keycloak (realm importado) e três instâncias da aplicação:

```bash
docker compose up --build
```

| Serviço | Endereço no host |
| --- | --- |
| Instâncias da aplicação | `http://localhost:8081`, `:8082`, `:8083` |
| PostgreSQL | `localhost:5432` (banco `wagering`) |
| LocalStack (SQS) | `http://localhost:4566` |
| Keycloak | `http://localhost:8180` (admin: `admin` / `admin`) |
| Swagger UI (documentação interativa) | `http://localhost:8089` |

Se alguma porta do host já estiver ocupada (por exemplo, outro Postgres na 5432), mude-a sem editar arquivos: `POSTGRES_PORT=55432 docker compose up --build` (também existem `LOCALSTACK_PORT` e `KEYCLOAK_PORT`).

Verificação automática do ambiente (migrations, instâncias, token e filas), a mesma executada no CI:

```bash
bash .github/scripts/smoke.sh
```

### API

| Endpoint | Papel | Para quê |
| --- | --- | --- |
| `POST /wallets` | `internal` | abrir uma carteira (a abertura entra no ledger) |
| `GET /wallets/{walletId}` | `internal` | ler a carteira |
| `GET /wallets/{walletId}/ledger` | `internal` | listar o ledger, com cursor opaco |
| `POST /wallets/{walletId}/reconciliation` | `internal` | conferir o saldo guardado contra o ledger |
| `POST /wagering/transactions` | `provider` | enviar uma operação (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`), com `Idempotency-Key` |
| `GET /wagering/transactions/{transactionId}` | `provider`, `internal` | ler uma transação pelo id |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | `provider`, `internal` | ler pelo id externo |
| `GET /health/live`, `GET /health/ready`, `GET /metrics`, `GET /openapi.yaml` | nenhum | saúde, métricas e o contrato |

O contrato da API é [api/openapi.yaml](api/openapi.yaml), escrito à mão, embutido no binário e servido em `GET /openapi.yaml`. A API usa JSON, valores monetários em string com duas casas (`"100.00"`) e erros no formato `{"error":{"code":...,"message":...}}`. Os endpoints de carteira exigem o papel `internal`.

Interativo: abra `http://localhost:8089`, clique em **Authorize**, informe `internal-service` e o secret `internal-service-local-secret` (fluxo `client_credentials`) e use **Try it out**.

Pela linha de comando:

```bash
TOKEN=$(curl -s -X POST http://localhost:8180/realms/wagering/protocol/openid-connect/token -d grant_type=client_credentials -d client_id=internal-service -d client_secret=internal-service-local-secret | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
```

```bash
curl -s -X POST localhost:8081/wallets -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"playerId":"7c9e6679-7425-40de-944b-e07fc1f90ae7","initialBalance":{"amount":"100.00","currency":"BRL"}}'
```

```bash
curl -s localhost:8082/wallets/<id-devolvido-acima> -H "Authorization: Bearer $TOKEN"
```

Uma operação de um provedor (o token é de `provider-a` e o `providerId` do corpo precisa ser o dele; `playerId` e `walletId` são os da carteira aberta acima):

```bash
PROVIDER_TOKEN=$(curl -s -X POST http://localhost:8180/realms/wagering/protocol/openid-connect/token -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
```

```bash
curl -s -X POST localhost:8082/wagering/transactions -H "Authorization: Bearer $PROVIDER_TOKEN" -H 'Idempotency-Key: provider-a:transaction-123' -H 'Content-Type: application/json' -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"<playerId>","walletId":"<walletId>","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'
```

Consultar o resultado, pelo id ou pelo id externo (um provedor só enxerga as próprias transações; as de outro provedor respondem `404`):

```bash
curl -s localhost:8081/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER_TOKEN"
```

```bash
curl -s localhost:8081/wagering/transactions/<transactionId-da-resposta> -H "Authorization: Bearer $PROVIDER_TOKEN"
```

Repetir o mesmo comando devolve o resultado guardado com `"idempotentReplay":true`; mudar o valor mantendo a chave dá `409`. `BET` debita, `WIN` credita e `LOSS` só registra o resultado (valor `0.00`, sem lançamento e sem mudar a versão da carteira). `REFUND` e `ROLLBACK` nomeiam a operação que desfazem em `referenceExternalTransactionId` (resolvida entre as transações do próprio provedor): o `REFUND` devolve uma `BET`, o `ROLLBACK` desfaz uma `BET`, um `WIN` ou um `REFUND` com o movimento contrário, e cada transação só pode ser revertida uma vez. Cada regra da referência recusa com o seu código (`REFERENCE_NOT_FOUND`, `REFERENCE_KIND_INVALID`, `REFERENCE_MISMATCH`, `REFERENCE_AMOUNT_MISMATCH`, `REFERENCE_ALREADY_REVERSED`, `REVERSAL_INSUFFICIENT_FUNDS`...). Um `WIN` pode citar a `BET` que paga. Se a referência ainda não chegou (a reversão foi enviada antes da `BET`, por exemplo), a operação é aceita com `202` e `PENDING_REFERENCE`, e um worker volta a tentá-la com espera crescente por até 10 tentativas ou 15 minutos; ela termina como `PROCESSED` ou como `REJECTED` (`REFERENCE_NOT_FOUND` se a referência nunca apareceu, `REFERENCE_NOT_PROCESSED` se apareceu e não completou), e o resultado se lê em `GET /wagering/transactions/<id>`.

A mesma operação também entra pela fila `wager-transactions.fifo` (corpo no formato da seção 10 do enunciado, `MessageGroupId` = id da carteira). O resultado é o mesmo do HTTP, inclusive a idempotência: a mensagem é apagada da fila só depois do commit, o que não pode ser tratado (JSON inválido, tipo desconhecido, campo inválido, chave ou `messageId` reutilizados com outro conteúdo) vai para `wager-transactions-dlq.fifo` com o motivo em `errorReason`, e uma falha do momento deixa a mensagem na fila para outra tentativa, com espera crescente.

O ledger de uma carteira (somente o serviço interno), da entrada mais antiga para a mais nova, com a abertura incluída; use o `nextCursor` de uma página como `cursor` da seguinte:

```bash
curl -s "localhost:8081/wallets/<id-da-carteira>/ledger?limit=50" -H "Authorization: Bearer $TOKEN"
```

Reconciliar uma carteira (somente o serviço interno): recalcula o saldo a partir do ledger e compara com o saldo guardado. Responde `200` mesmo quando há divergência, com `consistent:false` e a `difference` (guardado menos calculado); a divergência também vira um log `ERROR` e incrementa `reconciliation_divergences_total`. Nada é alterado.

```bash
curl -s -X POST localhost:8081/wallets/<id-da-carteira>/reconciliation -H "Authorization: Bearer $TOKEN"
```

Outros endpoints:

```bash
curl -s localhost:8081/health/live
```

```bash
curl -s localhost:8081/health/ready
```

```bash
curl -s localhost:8081/metrics
```

```bash
curl -s localhost:8081/openapi.yaml
```

Para rodar uma instância fora do Docker, suba só as dependências e use as variáveis do `.env.example`:

```bash
cp .env.example .env
```

```bash
make deps
```

```bash
make run
```

## Imagem publicada

A cada merge na `main`, depois do CI verde, a imagem multi-arquitetura (`linux/amd64` e `linux/arm64`) é publicada no GitHub Container Registry:

| Tag | Significado |
| --- | --- |
| `latest` | último commit da `main` |
| `sha-<7 caracteres>` | commit exato que o CI validou |
| `X.Y.Z` e `X.Y` | release, criada ao empurrar a tag git `vX.Y.Z` |

```bash
docker pull ghcr.io/guilhermefaglioni/backend-challenge-go:latest
```

> O pacote no GitHub Container Registry nasce privado. Se o `docker pull` responder `denied`, o dono do repositório precisa torná-lo público uma vez (GitHub, aba Packages, configurações do pacote, visibilidade), ou você pode fazer `docker login ghcr.io` com um token com `read:packages`.

A imagem sozinha não sobe: ela precisa de PostgreSQL, SQS e Keycloak, então o caminho suportado é o Compose abaixo. Um `docker run` só serve para ver a aplicação recusar uma configuração inválida:

```bash
docker run --rm ghcr.io/guilhermefaglioni/backend-challenge-go:latest
```

Para subir o ambiente com a imagem publicada, sem compilar:

```bash
APP_IMAGE=ghcr.io/guilhermefaglioni/backend-challenge-go:latest docker compose up --no-build
```

## Variáveis de ambiente

Todas estão em [.env.example](.env.example). A aplicação valida a configuração no boot e não sobe se algo estiver inválido.

| Variável | Obrigatória | Descrição |
| --- | --- | --- |
| `DATABASE_URL` | sim | Conexão como `wagering_app` (papel sem permissão de alterar o ledger) |
| `AWS_REGION` | sim | Região do SQS |
| `AWS_ENDPOINT_URL` | não | Endpoint do LocalStack; vazio usa a AWS real |
| `OIDC_ISSUER_URL` | sim | `iss` esperado nos tokens |
| `OIDC_DISCOVERY_URL` | não | Onde buscar metadados e JWKS; padrão é o issuer |
| `OIDC_AUDIENCE` | não | `aud` esperado (padrão `wallet-api`) |
| `SQS_INPUT_QUEUE`, `SQS_EVENTS_QUEUE` | não | Filas FIFO de entrada e de eventos |
| `SQS_DEAD_LETTER_QUEUE` | não | Fila FIFO para onde o consumidor envia o que nunca poderá ser tratado (`wager-transactions-dlq.fifo`) |
| `SQS_CONSUMER_WORKERS`, `SQS_CONSUMER_BATCH_SIZE`, `SQS_CONSUMER_POLL_WAIT` | não | Consumidor: laços de recebimento por instância (`4`), mensagens por recebimento (`10`, máximo do SQS) e espera do long polling (`20s`, máximo do SQS) |
| `SQS_CONSUMER_PROCESSING_TIMEOUT` | não | Prazo para tratar uma mensagem (`20s`); precisa ficar abaixo do visibility timeout da fila (30s) |
| `SQS_CONSUMER_RETRY_BACKOFF_BASE`, `SQS_CONSUMER_RETRY_BACKOFF_MAX` | não | Espera, em segundos inteiros, de uma mensagem que falhou por um motivo do momento: a base (`2s`) dobra a cada recebimento até o máximo (`60s`) |
| `PENDING_REFERENCE_MAX_ATTEMPTS`, `PENDING_REFERENCE_TTL` | não | Quanto uma operação espera pela referência: tentativas (`10`, a chegada conta como a primeira) e tempo (`15m`), o que vier primeiro |
| `PENDING_REFERENCE_BACKOFF_BASE`, `PENDING_REFERENCE_BACKOFF_MAX` | não | Intervalo entre tentativas: começa em `2s`, dobra a cada tentativa e para em `60s` (com jitter de ±20%) |
| `PENDING_REFERENCE_POLL_INTERVAL`, `PENDING_REFERENCE_BATCH_SIZE`, `PENDING_REFERENCE_LEASE` | não | Worker das referências pendentes: espera quando nada venceu (`500ms`), operações reservadas por vez (`10`) e quanto tempo uma reserva vale se a instância morrer (`30s`) |
| `OUTBOX_POLL_INTERVAL`, `OUTBOX_BATCH_SIZE`, `OUTBOX_LEASE` | não | Publicador da outbox: espera quando não há nada a enviar (`500ms`), eventos por lote (`10`, máximo do SQS) e por quanto tempo um evento reservado fica reservado (`30s`) |
| `CORS_ALLOWED_ORIGINS` | não | Origens de navegador autorizadas a chamar a API (separadas por vírgula, no formato `scheme://host[:porta]`). Vazio, o padrão, não autoriza nenhuma; o Compose autoriza o Swagger UI |
| `HTTP_ADDR`, `LOG_LEVEL`, `SHUTDOWN_TIMEOUT`, `DATABASE_MAX_CONNS`, `INSTANCE_ID` | não | Ajustes operacionais |

## Filas

O LocalStack executa [deploy/localstack/init/01-sqs.sh](deploy/localstack/init/01-sqs.sh) ao ficar pronto e cria:

- `wager-transactions.fifo`: entrada, visibility timeout de 30s, redrive para a DLQ após 5 recebimentos;
- `wager-transactions-dlq.fifo`: dead-letter queue;
- `wager-events.fifo`: eventos de integração publicados pela outbox.

## Migrations

Ficam em [migrations/](migrations/) no formato do `golang-migrate` (`000001_nome.up.sql` e `000001_nome.down.sql`, versões contíguas a partir de `000001`) e rodam automaticamente no `docker compose up` (serviço `migrate`, como dono do schema `wagering_owner`).

Aplicar tudo, e reverter a última migration (ou as `N` últimas):

```bash
make migrate-up
```

```bash
make migrate-down
```

```bash
make migrate-down N=2
```

Os alvos usam o serviço `migrate` do Compose, que alcança o PostgreSQL pela rede interna, como dono do schema (`wagering_owner`). Fora do Docker, com o [CLI do golang-migrate](https://github.com/golang-migrate/migrate) instalado e o PostgreSQL do Compose de pé (a porta do host é `POSTGRES_PORT`, 5432 por padrão), o mesmo comando é:

```bash
migrate -path migrations -database "postgres://wagering_owner:owner_local_pw@localhost:5432/wagering?sslmode=disable" down 2
```

(credenciais apenas locais, as mesmas do `docker-compose.yml`).

### Como documentamos o rollback

Todo `.down.sql` começa com um cabeçalho de comentários, com estes quatro campos, nesta ordem, que dizem o que acontece **antes** de alguém reverter:

| Campo | O que diz |
| --- | --- |
| `Undoes` | o que a migration desfaz |
| `Loses` | o que se perde ao reverter: dados **e proteções** (constraints, triggers, índices, permissões). Um rollback que apaga dados ou remove proteções precisa dizer isso aqui; `nothing` só vale quando for verdade |
| `Requires` | o que precisa estar de pé (ou ausente) antes de reverter, como a ordem em relação a outras migrations |
| `Command` | o comando exato, com `migrate ... down` |

```sql
-- Undoes: the creation of the ledger table and its triggers.
-- Loses: ALL ledger entries, that is, the financial history. Use only before real data exists.
-- Requires: migrations that depend on the ledger are reverted first.
-- Command: make migrate-down N=1
DROP TABLE wallet_ledger_entries;
```

Um campo pode continuar nas linhas seguintes, começando com `--` e dois espaços ou mais. Um teste (`go test ./test/migrations/`, no CI) reprova o build se faltar o par `up`/`down`, se houver salto ou repetição de versão, se o cabeçalho faltar, estiver incompleto ou fora de ordem, ou se um `.down.sql` que apaga dados (`DROP TABLE`, `TRUNCATE`, `DELETE FROM`) ou remove proteções (`DROP TRIGGER`, `DROP INDEX`, `DROP CONSTRAINT`, `DROP FUNCTION`, `REVOKE`) disser em `Loses` que não perde nada (`nothing`, `none`, `no data`, `no loss`, `n/a`). Comandos só mencionados em comentários não contam.

## Autenticação

O Keycloak importa o realm [deploy/keycloak/wagering-realm.json](deploy/keycloak/wagering-realm.json) com três clients `client_credentials`:

| Client | Papel | `provider_id` |
| --- | --- | --- |
| `provider-a` | `provider` | `provider-a` |
| `provider-b` | `provider` | `provider-b` |
| `internal-service` | `internal` | não tem |

Obter um token (os secrets são apenas locais):

```bash
curl -s -X POST http://localhost:8180/realms/wagering/protocol/openid-connect/token -d grant_type=client_credentials -d client_id=provider-a -d client_secret=provider-a-local-secret
```

A API valida esses tokens localmente (assinatura RS256, emissor, audiência `wallet-api`, expiração) e só sobe depois de descobrir o Keycloak; o desenho está no `ARCHITECTURE.md`, seção 13. Toda rota de negócio é registrada com os papéis que podem chamá-la (`provider` ou `internal`), e um teste reprova rotas sem autorização.

## Testes

Testes unitários (rápidos, sem Docker):

```bash
go test ./...
```

```bash
go test -race ./...
```

```bash
go vet ./...
```

### Cobertura

O CI combina a cobertura dos testes unitários, de integração e dos processos da camada multi-instância num único relatório (na página do run, aba Summary) e exige 90% em cada pacote de `internal/domain/`. Para gerar localmente um relatório de uma camada:

```bash
mkdir -p /tmp/cover-unit && go test -cover -coverpkg=./internal/...,./cmd/... ./... -args -test.gocoverdir=/tmp/cover-unit
THRESHOLD=0 COVER_DIRS=/tmp/cover-unit bash .github/scripts/coverage-report.sh
```

`THRESHOLD=0` desliga o gate: com os dados de uma única camada, o domínio quase sempre fica abaixo de 90%, porque as outras camadas ainda não contribuíram.

### Integração (infraestrutura real)

Sobem PostgreSQL, LocalStack (SQS) e Keycloak em containers com `testcontainers-go`, usando as mesmas imagens, migrations, script de filas e realm do `docker compose`. Ficam atrás da build tag `integration`, então `go test ./...` não os executa. Exigem Docker em execução:

```bash
go test -tags integration -race -count=1 -shuffle=on ./test/...
```

O que cobrem: migrations (aplicar, reverter e reaplicar) e cada constraint e trigger do schema tentados por SQL direto, privilégios do papel `wagering_app`, a autenticação contra o Keycloak real, o contrato HTTP de todos os endpoints validado contra o `api/openapi.yaml`, o isolamento entre provedores, cada caso de uso contra o banco real (inclusive disputas concorrentes), o consumidor SQS e a inbox contra o LocalStack (com cópias de mensagem de `MessageDeduplicationId` diferente, para provar que a deduplicação é da aplicação), o publicador da outbox, o worker das referências pendentes, a reconciliação, e o início e encerramento da composição Fx contra a infraestrutura real (ordem de shutdown e ausência de goroutines remanescentes).

Se um teste falhar, os logs dos containers são gravados em `INTEGRATION_LOG_DIR` (o CI os publica como artefato por 7 dias):

```bash
INTEGRATION_LOG_DIR=/tmp/integration-logs go test -tags integration ./test/...
```

### Múltiplas instâncias e injeção de falhas

Executam o binário da aplicação como **processos separados do sistema operacional** (três ou mais, cada um com suas conexões e memória) contra PostgreSQL e LocalStack reais. O binário é compilado pelo próprio teste com `-race`, instrumentação de cobertura e a build tag `faultinject`, que habilita os pontos de falha. Ficam atrás da build tag `multiinstance` e exigem Docker e uma toolchain Go com cgo (por causa do `-race`):

```bash
go test -tags multiinstance -race -count=1 -shuffle=on ./test/multiinstance/
```

Cobrem: três instâncias prontas; uma instância morta com `SIGKILL` sem afetar as outras e voltando no mesmo endereço; `SIGTERM` encerrando limpo; os pontos de falha (código `77`); o consumidor morto entre o commit e a remoção da mensagem; dois publicadores disputando a outbox e recuperando uma publicação interrompida; uma espera pela referência cujo processo morre, cujo worker morre segurando a reserva, que atravessa o reinício de todos os processos ou que expira; e as garantias financeiras da seção seguinte. Qualquer corrida de dados relatada por um processo reprova o teste.

**Pontos de falha** (`internal/platform/faultinject`): um processo sai abruptamente quando seu nome está em `FAULT_POINTS` (lista separada por vírgula). Só existem em binários compilados com `-tags faultinject`; sem a tag, `Hit` não faz nada e a variável é ignorada (o binário de produção nem contém o código). Para criar um ponto, declare a constante em `points.go`, registre-a em `faultinject_on.go` e chame `faultinject.Hit(...)` onde o processo deve cair.

Em caso de falha, os logs de cada instância e de cada container vão para `INTEGRATION_LOG_DIR`. Os dados de cobertura dos processos vão para `MULTIINSTANCE_COVERDIR`.

### Garantias financeiras com três processos

`test/multiinstance/guarantees_test.go` roda os cenários obrigatórios do desafio pela API HTTP contra três processos reais (cada um com suas conexões e memória), com o detector de corrida: a mesma aposta enviada 50 vezes em paralelo é debitada uma vez; 100,00 com duas apostas de 80,00 ao mesmo tempo dão uma processada, uma `INSUFFICIENT_FUNDS`, saldo 20,00 e um único débito; operações em carteiras diferentes não esperam umas pelas outras; e, depois de reiniciar todas as instâncias, o que foi processado continua sendo reproduzido com a resposta original e o saldo igual. Cada cenário termina conferindo que o saldo é a soma dos créditos menos os débitos do ledger, e pedindo a outro processo que diga o mesmo pelo endpoint de reconciliação. Esta suíte já encontrou uma corrida real (ver ARCHITECTURE.md, seção 8).

### Teste de carga

Com o Compose de pé, `make loadtest` mede carteiras independentes contra uma carteira só (1, 4, 16 e 64 workers) e o custo dos triggers de coerência, e reporta throughput, p50/p95/p99, erros, conflitos de concorrência e o atraso da outbox. O método, o ambiente e os resultados registrados estão em [docs/LOAD_TEST.md](docs/LOAD_TEST.md).

### Proteção da `main`

As regras do repositório (PR obrigatório, checks exigidos, somente squash, tags `v*` restritas) estão em [.github/rulesets](.github/rulesets). Para ver o que seria aplicado, sem alterar nada, e para aplicar (exige ser administrador do repositório):

```bash
bash .github/scripts/apply-rulesets.sh
```

```bash
bash .github/scripts/apply-rulesets.sh --apply
```

### Execução noturna

O workflow `Nightly` repete as camadas de integração e multi-instância contra a `main` toda noite com `-count=5`. Uma falha abre (ou atualiza) uma issue com o label `nightly-failure`, citando a camada, o link do run e a seed do `-shuffle`. O `go test` sorteia uma seed por execução, então as 5 repetições de uma noite compartilham a mesma ordem; cada noite traz uma ordem nova. Para reproduzir localmente:

```bash
go test -tags multiinstance -race -count=5 -shuffle=<seed> ./test/multiinstance/
```

Também pode ser disparado à mão na aba Actions (`count` e `simulate_failure`).
