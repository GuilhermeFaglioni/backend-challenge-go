# Wagering service

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores, com várias instâncias em paralelo e tolerância a falhas.

- Enunciado do desafio: [docs/CHALLENGE.md](docs/CHALLENGE.md)
- Decisões técnicas: [ARCHITECTURE.md](ARCHITECTURE.md) e os registros curtos em [docs/adr](docs/adr)
- Ordem de construção e definição de pronto: [docs/ROADMAP.md](docs/ROADMAP.md)
- Vocabulário do projeto: [CONTEXT.md](CONTEXT.md)

> **Status: infraestrutura pronta, aplicação por construir.** A composição Fx, a configuração, os health checks, as métricas, o ambiente Docker e todo o CI/CD (testes unitários, de integração e multi-instância, cobertura, publicação da imagem e proteção da `main`) estão prontos. Domínio, schema, casos de uso, autenticação, consumidor SQS e workers seguem o [roadmap](docs/ROADMAP.md) e ainda não foram implementados. Este README será completado conforme cada parte for entregue.

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

Se alguma porta do host já estiver ocupada (por exemplo, outro Postgres na 5432), mude-a sem editar arquivos: `POSTGRES_PORT=55432 docker compose up --build` (também existem `LOCALSTACK_PORT` e `KEYCLOAK_PORT`).

Verificação automática do ambiente (migrations, instâncias, token e filas), a mesma executada no CI:

```bash
bash .github/scripts/smoke.sh
```

Endpoints disponíveis no scaffold:

```bash
curl -s localhost:8081/health/live
```

```bash
curl -s localhost:8081/health/ready
```

```bash
curl -s localhost:8081/metrics
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
| `HTTP_ADDR`, `LOG_LEVEL`, `SHUTDOWN_TIMEOUT`, `DATABASE_MAX_CONNS`, `INSTANCE_ID` | não | Ajustes operacionais |

## Filas

O LocalStack executa [deploy/localstack/init/01-sqs.sh](deploy/localstack/init/01-sqs.sh) ao ficar pronto e cria:

- `wager-transactions.fifo`: entrada, visibility timeout de 30s, redrive para a DLQ após 5 recebimentos;
- `wager-transactions-dlq.fifo`: dead-letter queue;
- `wager-events.fifo`: eventos de integração publicados pela outbox.

## Migrations

Ficam em [migrations/](migrations/) no formato do `golang-migrate` e rodam automaticamente no `docker compose up` (serviço `migrate`, como dono do schema `wagering_owner`).

```bash
make migrate-up
```

```bash
make migrate-down
```

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

A validação dos tokens na API ainda não foi implementada.

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

O que cobrem hoje: migrations (aplicar, reverter e reaplicar), privilégios do papel `wagering_app`, início e encerramento da composição Fx contra a infraestrutura real (ordem de shutdown e ausência de goroutines remanescentes), readiness acompanhando Postgres e SQS, e o provisionamento do Keycloak e das filas.

Se um teste falhar, os logs dos containers são gravados em `INTEGRATION_LOG_DIR` (o CI os publica como artefato por 7 dias):

```bash
INTEGRATION_LOG_DIR=/tmp/integration-logs go test -tags integration ./test/...
```

### Múltiplas instâncias e injeção de falhas

Executam o binário da aplicação como **processos separados do sistema operacional** (três ou mais, cada um com suas conexões e memória) contra PostgreSQL e LocalStack reais. O binário é compilado pelo próprio teste com `-race`, instrumentação de cobertura e a build tag `faultinject`, que habilita os pontos de falha. Ficam atrás da build tag `multiinstance` e exigem Docker e uma toolchain Go com cgo (por causa do `-race`):

```bash
go test -tags multiinstance -race -count=1 -shuffle=on ./test/multiinstance/
```

Cobrem hoje: três instâncias prontas; uma instância morta com `SIGKILL` sem afetar as outras e voltando no mesmo endereço; `SIGTERM` encerrando limpo (servidor antes do pool) e gravando cobertura; um ponto de falha derrubando o processo com o código `77`; e a recusa de iniciar com um nome de ponto de falha inexistente. Qualquer corrida de dados relatada por um processo reprova o teste.

**Pontos de falha** (`internal/platform/faultinject`): um processo sai abruptamente quando seu nome está em `FAULT_POINTS` (lista separada por vírgula). Só existem em binários compilados com `-tags faultinject`; sem a tag, `Hit` não faz nada e a variável é ignorada (o binário de produção nem contém o código). Para criar um ponto, declare a constante em `points.go`, registre-a em `faultinject_on.go` e chame `faultinject.Hit(...)` onde o processo deve cair.

Em caso de falha, os logs de cada instância e de cada container vão para `INTEGRATION_LOG_DIR`. Os dados de cobertura dos processos vão para `MULTIINSTANCE_COVERDIR`.

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
