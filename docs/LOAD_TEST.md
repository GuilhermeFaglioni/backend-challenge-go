# Teste de carga

Um teste de carga reproduzível, que mostra com números o que o desenho promete e o que ele não promete. Não há meta de throughput: o objetivo é **ver** o sistema, e isso incluiu achar um gargalo (abaixo). Os números são de um notebook rodando tudo ao mesmo tempo (o gerador de carga, três instâncias, PostgreSQL, Keycloak e LocalStack dentro do Docker), então valem como ordem de grandeza e como comparação entre cenários, não como capacidade de produção.

## Como rodar

Com o ambiente do Compose de pé e as três instâncias prontas:

```bash
docker compose up --build -d
```

```bash
make loadtest
```

(`make loadtest` é `bash test/load/run.sh`; se a porta 5432 do host estiver ocupada, suba o Compose com `POSTGRES_PORT=55432` e rode `POSTGRES_PORT=55432 make loadtest`.) Os parâmetros são variáveis de ambiente, listadas no cabeçalho do script: `CONCURRENCY`, `SWEEP`, `DURATION`, `WARMUP`, `DRAIN`, `TARGETS`, `KEYCLOAK_URL`. O programa que gera a carga é [cmd/loadtest](../cmd/loadtest/main.go) e pode ser usado sozinho (`go run ./cmd/loadtest -h`). Uma execução completa leva cerca de 15 minutos.

## Metodologia

- **Carga.** N *workers* concorrentes, cada um em laço fechado (envia a próxima operação quando a anterior responde), distribuindo as requisições entre as três instâncias. A mistura é de dez em dez: sete `BET` de 1,00, dois `WIN` de 1,00 e um `LOSS`, de modo que o saldo (1.000.000,00 na abertura) mal se move. Cada operação tem chave de idempotência e id externo próprios. Nada é repetido: é carga de operações novas.
- **Cenários.** *Independentes*: cada worker tem a sua carteira, então as operações nunca esperam o lock umas das outras. *Uma carteira*: todos os workers usam a mesma, serializados pelo lock (ADR [0002](adr/0002-per-wallet-pessimistic-lock.md)).
- **Medição.** Aquecimento de 3 s, não reportado; depois a janela medida (20 s no primeiro conjunto, 15 s nas repetições). Latência medida no cliente, de ponta a ponta (HTTP, autorização, transação, resposta); percentis exatos sobre todas as amostras. Conflitos de concorrência: o crescimento de `db_transaction_retries_total` somado nas três instâncias. Atraso da outbox: lido **direto da tabela** a cada 500 ms (quantos eventos esperam, e a idade do mais antigo), porque o medidor das instâncias só se atualiza a cada 2 s. Antes de cada rodada a outbox é esvaziada (o que sobra é marcado como publicado, dado de teste) e o fim da rodada espera a outbox alcançar a carga, até o limite de `DRAIN`.
- **Verificação.** Todas as rodadas terminam reconciliando **cada carteira usada**, pelo endpoint de reconciliação. Em nenhuma das rodadas deste documento houve divergência.
- **Custo dos triggers de coerência.** Os dois triggers adiados que conferem saldo e ledger no commit são desligados e religados pelo dono do schema (`ALTER TABLE ... DISABLE TRIGGER`), **só para medir**; o script os religa ao sair, qualquer que seja o resultado.

## Ambiente

15/10/2026, macOS arm64 (Apple Silicon, 8 CPUs lógicas), Docker Desktop 4.37.2 (Docker 27.4.0), Go 1.27.2, PostgreSQL 16, LocalStack 3.8.1, Keycloak 26.0.7, três instâncias da aplicação com as configurações padrão (`DATABASE_MAX_CONNS` 10 por instância).

## Resultados

### Carteiras independentes contra uma carteira só, por número de workers

Saída completa em [load-test/2026-10-15.md](load-test/2026-10-15.md) (janela de 20 s; ambiente recém-criado, banco crescendo ao longo das doze rodadas).

| workers | carteiras | req/s | p50 ms | p95 ms | p99 ms | máx ms | conflitos absorvidos |
| ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | independentes | 660 | 1,4 | 2,2 | 3,6 | 100,5 | 0 |
| 1 | uma só | 664 | 1,4 | 2,3 | 3,9 | 24,8 | 0 |
| 4 | independentes | 522 | 6,7 | 13,7 | 21,0 | 91,4 | 0 |
| 4 | uma só | 441 | 8,2 | 16,2 | 23,5 | 75,0 | 0 |
| 16 | independentes | 981 | 13,6 | 33,7 | 62,4 | 295,2 | 0 |
| 16 | uma só | 289 | 15,6 | 229,2 | 436,1 | 1306,2 | 0 |
| 64 | independentes | 1058 | 55,1 | 120,0 | 181,0 | 318,3 | 0 |
| 64 | uma só | 339 | 140,3 | 506,9 | 755,0 | 1329,5 | 0 |

Com um worker as duas formas são idênticas, como devem ser. Com 16 e 64 workers as carteiras independentes entregam cerca de **3 vezes** o throughput da carteira única, e a carteira única mostra o que a serialização custa na cauda: p99 de 436 ms e 755 ms contra 62 ms e 181 ms. As carteiras independentes avançam em paralelo; a carteira única satura em algumas centenas de operações por segundo e o excesso de workers vira só fila (a mediana sobe de 15,6 ms para 140 ms sem ganhar throughput).

### Repetições, para saber o quanto confiar

Quatro rodadas intercaladas de 15 s com 16 workers, num ambiente recriado (saída em [load-test/2026-10-15-repetitions.md](load-test/2026-10-15-repetitions.md)):

| | req/s por rodada | mediana | p99 por rodada (ms) | mediana |
| --- | --- | ---: | --- | ---: |
| carteiras independentes | 416, 479, 1090, 1200 | 784 | 99,9; 378,6; 38,8; 34,5 | 69 |
| uma carteira | 386, 129, 415, 34 | 258 | 330; 1284; 313; 3068 | 807 |
| triggers ligados (32 carteiras) | 884, 1267, 1252, 1171 | 1212 | 87; 32; 32; 43 | 38 |
| triggers desligados (32 carteiras) | 824, 1234, 1293, 1236 | 1235 | 114; 34; 30; 34 | 34 |

O que isto autoriza dizer, e o que não autoriza:

- **A ordem de grandeza e o sentido da comparação se mantêm:** a carteira única é várias vezes mais lenta que as independentes e tem a cauda muito pior. Mas o ruído entre rodadas idênticas é grande (de 416 a 1200 req/s para a mesma carga; as primeiras rodadas de um ambiente novo são as mais lentas, com cache e planos frios). Os números de uma rodada isolada não devem ser citados sozinhos, e os da carteira única são especialmente instáveis (34 a 415 req/s).
- **O custo dos triggers de coerência não é distinguível do ruído neste ambiente.** Nas rodadas já aquecidas a mediana é 1212 req/s com os triggers e 1235 sem eles (menos de 2% de diferença, bem abaixo da variação entre rodadas iguais). O primeiro conjunto sugeria 20% a 40%, mas era um efeito de ordem e de aquecimento: é por isso que o script alterna ligado e desligado. O que se pode afirmar é que, com as consultas indexadas que eles fazem, o custo não aparece a ~1.200 req/s; não que seja zero.
- **Conflitos de concorrência: nenhum.** Em todas as rodadas, `db_transaction_retries_total` (`concurrent_update`, `deadlock`, `serialization_failure`) não saiu de zero. É o esperado: o lock pessimista por carteira e a ordem de aquisição fixa fazem as operações esperarem em fila em vez de colidirem, e a espera aparece como latência, não como retentativa.
- **Erros: nenhum.** Todas as respostas medidas foram `200`; nenhum erro de transporte.

## O que a medição achou: a outbox atrás da carga

A cada operação processada a aplicação grava dois eventos na outbox, então a 1.000 req/s são ~2.000 eventos por segundo. A primeira medição, ainda sem as correções abaixo, mostrou o problema (16 workers, 15 s, ambiente recém-criado, 60 s de espera depois da carga):

| | req/s | p50 / p99 ms | eventos escritos | fila máxima | idade do mais antigo | alcançou a carga em 60 s? |
| --- | ---: | --- | ---: | ---: | ---: | --- |
| independentes, antes | 453 | 29 / 160 | 12.913 | 16.513 | 16,8 s | não |
| uma carteira, antes | 75 | 126 / 1.112 | 2.110 | 2.263 | 16,4 s | não |
| independentes, depois | 439 | 32 / 134 | 12.498 | 15.478 | 16,3 s | sim, em 17,6 s |
| uma carteira, depois | 295 | 36 / 251 | 8.396 | 9.503 | 17,0 s | não |

A causa, vista nos logs do publicador, era uma ineficiência real: a ordem por carteira exige que um evento só seja reservado quando todos os anteriores da mesma carteira estiverem publicados, então cada ciclo reserva no máximo **um evento por carteira**; e o laço só continuava sem esperar quando o lote vinha **cheio**, de modo que, com poucas carteiras ativas, cada ciclo era seguido de 500 ms de espera: cerca de **2 eventos por segundo por carteira** (medido: 5,6 eventos/s no total, para uma carteira, com três publicadores). Corrigido neste mesmo PR: o publicador agora só espera quando um ciclo não encontra nada (teste em `internal/adapters/outbox`, visto falhar com a condição antiga). A vazão por carteira foi para cerca de **20 eventos/s** (um ciclo de ~50 ms entre reservar, enviar ao SQS e marcar), a escrita da carteira única deixou de ser engolida pelos publicadores (75 para 295 req/s nesta comparação), e a fila das carteiras independentes passou a ser alcançada depois de a carga parar. Todas as tabelas de resultados deste documento são **depois** da correção.

Fica o limite que isto revela, e ele continua valendo: **uma carteira pode gerar eventos mais rápido do que a outbox os publica** (uma carteira a 150 req/s gera ~300 eventos/s contra ~20 publicados), então, enquanto durar a carga, a fila e a idade do evento mais antigo crescem (16 a 22 s nas rodadas de 20 s, e a maioria não alcançou a carga dentro dos 45 s de espera do conjunto principal). Os eventos não se perdem e a ordem é preservada (a garantia está na ordem, não na vazão), e o atraso aparece nas métricas (`outbox_lag_seconds`); mas é um limite de capacidade por carteira. O caminho para removê-lo, não feito aqui, é reservar e enviar vários eventos consecutivos da mesma carteira no mesmo lote (o FIFO preserva a ordem dentro de um grupo), cuidando do que acontece se só parte do lote for aceita. Uma comparação com os publicadores parados (intervalo de 1 h) mostrou que, a 16 workers e carteiras independentes, a vazão da escrita não depende de eles estarem rodando (1919 contra 1849 req/s, dentro do ruído).

## Limitações desta medição

- Um notebook, com o gerador de carga competindo pelos mesmos recursos que o sistema medido; o ruído entre rodadas idênticas chega a ~3×.
- Laço fechado: a latência medida é a de um sistema em que o cliente espera, não a de uma chegada com taxa fixa.
- Operações sempre novas; replays e conflitos de idempotência, que são mais baratos, não entram.
- A fila de saída é o LocalStack, não o SQS real.
- Sem janela de estabilização longa nem repetição suficiente para intervalos de confiança.
