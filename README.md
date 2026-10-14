# Wagering service

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores, com várias instâncias em paralelo e tolerância a falhas.

- Enunciado do desafio: [docs/CHALLENGE.md](docs/CHALLENGE.md)
- Decisões técnicas: [ARCHITECTURE.md](ARCHITECTURE.md)

> **Status: scaffold.** A composição Fx, a configuração, os health checks, as métricas e o ambiente Docker estão prontos. Domínio, schema, casos de uso, autenticação, consumidor SQS e workers ainda não foram implementados. Este README será completado conforme cada parte for entregue.

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

```bash
go test ./...
```

```bash
go test -race ./...
```

```bash
go vet ./...
```

Os testes de integração (build tag `integration`), de múltiplas instâncias e de injeção de falhas serão documentados aqui quando forem implementados.
