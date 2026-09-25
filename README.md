# Jungle Wallet

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) recebidas por **HTTP** e **SQS**, movimentando carteiras de jogadores com consistência em ambiente distribuído.

- Composição com **Uber Fx**; **PostgreSQL** com migrations **goose**.
- Autenticação **OAuth 2.0 / OIDC** com **Keycloak** (`client_credentials`).
- **SQS** (LocalStack) para entrada de operações e publicação de eventos, com **transactional outbox**.
- Idempotência, ledger append-only, lock por carteira e deduplicação durável no banco: nenhuma garantia depende de memória local ou de uma única instância.

As decisões de projeto estão em **[ARCHITECTURE.md](ARCHITECTURE.md)**.

---

## Pré-requisitos

- **Docker** com **Docker Compose v2** (Docker Desktop no Windows/macOS). Reserve ~4 GB de disco para as imagens.
- **Go 1.27.1**, apenas para rodar testes e ferramentas fora do Docker.

## Subir o ambiente

```bash
docker compose up --build
```

Na ordem:

1. `postgres`, `keycloak` (realm `wallet` importado de `deploy/keycloak/`) e `localstack` (filas criadas por `deploy/localstack/init-queues.sh`);
2. `migrate` aplica as migrations e termina;
3. **três instâncias da API** (`api`, `api-2`, `api-3`) sobem depois que tudo acima está saudável.

| Serviço | Endereço no host |
|---|---|
| API (3 instâncias) | http://localhost:8080, http://localhost:8082, http://localhost:8083 |
| Keycloak | http://localhost:8081 (admin: `admin` / `admin`) |
| LocalStack (SQS) | http://localhost:4566 |
| PostgreSQL | `localhost:5433`, usuário/senha/banco `apostas` |

Verificar:

```bash
curl http://localhost:8080/health/ready
# {"checks":{"postgres":"UP","sqs":"UP"},"status":"UP"}
```

Parar: `docker compose down`. Parar e **apagar os dados**: `docker compose down -v`.

## Credenciais de exemplo

Clients `client_credentials` provisionados no realm `wallet` (apenas para ambiente local):

| client_id | client_secret | Identidade |
|---|---|---|
| `wallet-internal` | `wallet-internal-secret` | serviço interno: abre, consulta e reconcilia carteiras |
| `provider-a` | `provider-a-secret` | provedor `provider-a` |
| `provider-b` | `provider-b-secret` | provedor `provider-b` |
| `untrusted-client` | `untrusted-client-secret` | sem permissões (para testar autorização) |

O `providerId` autorizado vem do token (claim `provider_id`), nunca do corpo da requisição. Tokens valem 5 minutos.

## Usar a API

### Postman

Importe **`postman/jungle-wallet.postman_collection.json`** e execute as pastas em ordem (ou pelo *Collection Runner*). Tokens e ids são salvos automaticamente em variáveis da collection. A collection cobre carteira, operações, replay, conflitos, casos de segurança e o fluxo pelo SQS.

### curl (bash / Git Bash)

```bash
token() {
  curl -s -X POST http://localhost:8081/realms/wallet/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" \
    | sed -E 's/.*"access_token":"([^"]+)".*/\1/'
}
INTERNAL=$(token wallet-internal)
PROVIDER_A=$(token provider-a)
PLAYER=0192f28f-5dc0-7d58-bdb2-6a9c8e0f1234

# Abrir carteira (serviço interno)
curl -s -X POST http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}"
# -> 201 {"id":"<walletId>", ... "balance":{"amount":"100.00","currency":"BRL"}, "version":1}

WALLET=<walletId da resposta>

# Aposta (provedor). Repetir o comando com o mesmo $EXT devolve 200 com
# idempotentReplay=true; a mesma chave com outro conteúdo devolve 409.
EXT=transaction-$(date +%s)
curl -s -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:$EXT" \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$EXT\",
       \"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",
       \"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
```

Valores monetários são sempre **strings decimais** (`"25.00"`); um número JSON (`25.00`) é recusado com `400`.

### Endpoints

| Rota | Quem |
|---|---|
| `POST /wallets` | interno |
| `GET /wallets/{walletId}` | interno |
| `GET /wallets/{walletId}/ledger?cursor=&limit=` | interno |
| `POST /wallets/{walletId}/reconciliation` | interno |
| `POST /wagering/transactions` (header `Idempotency-Key`) | provedor |
| `GET /wagering/transactions/{transactionId}` | provedor (próprias) ou interno |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | o próprio provedor ou interno |
| `GET /health/live`, `GET /health/ready` | público |

Status de `POST /wagering/transactions`: `201` processada, `200` replay, `202` aguardando a transação de referência, `422` rejeitada por regra de negócio (ex.: saldo insuficiente). Erros: `400` entrada inválida, `401` sem autenticação válida, `403` sem permissão, `404` não encontrado, `409` conflito de idempotência, `503` indisponibilidade transitória (com `Retry-After`). Detalhes no [ARCHITECTURE.md](ARCHITECTURE.md#9-api-http).

### SQS

Fila de entrada: `wager-operations.fifo` (FIFO; `MessageGroupId` = id da carteira). O token do provedor vai no **atributo de mensagem `Authorization`**:

```bash
EXT2=transaction-$(date +%s)-sqs
BODY='{"messageId":"msg-'$EXT2'","type":"WagerOperationRequested","occurredAt":"2026-09-25T12:00:00Z",
"data":{"idempotencyKey":"provider-a:'$EXT2'","providerId":"provider-a",
"externalTransactionId":"'$EXT2'","playerId":"'$PLAYER'","walletId":"'$WALLET'",
"roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"10.00","currency":"BRL"}}}'

MSYS_NO_PATHCONV=1 docker compose exec -T localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-operations.fifo \
  --message-group-id "$WALLET" --message-deduplication-id "$EXT2" \
  --message-body "$BODY" \
  --message-attributes "{\"Authorization\":{\"DataType\":\"String\",\"StringValue\":\"Bearer $PROVIDER_A\"}}"
```

(`MSYS_NO_PATHCONV=1` só é necessário no Git Bash do Windows.)

- Mensagens com falha permanente (token inválido, entrada inválida, provedor não autorizado, conflito) vão para `wager-operations-dlq.fifo` com `failureCode` e `failureReason`, sem o token.
- Eventos de integração (`WagerTransactionProcessed`, `WalletBalanceChanged`, ...) são publicados em `wallet-events.fifo`:

```bash
docker compose exec -T localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10
```

### Logs

Cada instância escreve logs JSON na saída padrão: uma linha por requisição (método, rota, status, duração, `correlationId`), sem corpo nem credenciais. O `correlationId` também volta no header `X-Correlation-ID` e no corpo dos erros.

```bash
docker compose logs -f api api-2 api-3
```

## Testes

| Nível | Pacotes | Dependências |
|---|---|---|
| Unidade | `internal/domain`, `internal/config`, `internal/worker` | nenhuma |
| Integração | `internal/app`, `internal/infra/...`, `internal/bootstrap` | PostgreSQL, Keycloak e LocalStack reais |
| Ponta a ponta | `internal/e2e` (3 instâncias da API) | stack completa do Compose |

Nenhum teste de integração substitui PostgreSQL, SQS ou IdP por mocks. Cada teste de integração cria um banco próprio (e filas próprias no LocalStack) e os remove ao final. Sem as variáveis abaixo, os testes de integração são **pulados** (os de unidade rodam sempre).

Com o ambiente no ar (`docker compose up --build`):

```bash
export TEST_DATABASE_URL="postgres://apostas:apostas@localhost:5433/postgres?sslmode=disable"
export TEST_KEYCLOAK_URL="http://localhost:8081"
export TEST_SQS_ENDPOINT="http://localhost:4566"
export TEST_API_URLS="http://localhost:8080,http://localhost:8082,http://localhost:8083"

go test ./...
go test -race ./...
go vet ./...
gofmt -l .        # sem saída = tudo formatado
```

### Windows

`go test -race` exige cgo (compilador C). Sem ele, e também se o **Smart App Control** bloquear executáveis de teste, rode pelo container Go (Git Bash):

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W):/src" -w /src \
  -v gomodcache:/go/pkg/mod -v gobuildcache:/root/.cache/go-build \
  -e TEST_DATABASE_URL="postgres://apostas:apostas@host.docker.internal:5433/postgres?sslmode=disable" \
  -e TEST_KEYCLOAK_URL="http://host.docker.internal:8081" \
  -e TEST_SQS_ENDPOINT="http://host.docker.internal:4566" \
  -e TEST_API_URLS="http://host.docker.internal:8080,http://host.docker.internal:8082,http://host.docker.internal:8083" \
  golang:1.27.1 go test -race ./...
```

## Configuração

Todas as variáveis, com valores locais de exemplo, estão em **[.env.example](.env.example)**. Valores ausentes ou inválidos impedem a subida (a aplicação lista todos os erros e termina). No Compose, a configuração das instâncias está no bloco `x-api` do `docker-compose.yml`.

Migrations manuais (fora do Compose):

```bash
DATABASE_URL="postgres://apostas:apostas@localhost:5433/apostas?sslmode=disable" go run ./cmd/migrate up
# também: down, status
```

## Estrutura

```
cmd/api            entrada da aplicação (Fx)
cmd/migrate        migrations (goose)
internal/domain    regras de negócio, dinheiro, eventos (sem dependências de infraestrutura)
internal/app       casos de uso e portas (Store, EventPublisher); relay da outbox
internal/infra     postgres (pgx, SQL explícito), httpapi (chi), auth (OIDC), sqs
internal/bootstrap módulos Fx e ciclo de vida
internal/worker    laço de worker com encerramento observável
internal/e2e       testes contra as 3 instâncias
migrations/        SQL das migrations
deploy/            realm do Keycloak e criação das filas
postman/           collection para testes manuais
```
