---
status: accepted
---

# Dinheiro como `int64` em centavos, com moeda ISO 4217 e escala fixa de duas casas

`Money` guarda um `int64` em unidades mínimas e o código da moeda, persistido como `BIGINT` e `CHAR(3)`. O parsing aceita só o formato `^(0|[1-9][0-9]*)\.[0-9]{2}$`, sem formas equivalentes, e `Add`, `Sub` e `Neg` verificam overflow em vez de deixar o valor dar a volta.

## Considered Options

- **Biblioteca decimal ou `NUMERIC`:** precisão exata sem limite prático, mas adiciona dependência e alocação, e deixa a escala solta, o que obriga a normalizar entradas equivalentes (`25`, `25.0`, `25.00`) antes do hash de idempotência.
- **`float32`/`float64`:** proibido pelo enunciado em qualquer etapa.

## Consequences

- O limite é cerca de 92 quatrilhões de unidades; o overflow é tratado à mão e coberto por testes.
- Moedas com escala diferente de duas casas (JPY, KWD) não são suportadas.
- Como só existe uma forma textual válida, o hash canônico da idempotência não precisa de normalização, e o `BIGINT` preserva o valor exatamente.
