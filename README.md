# Jungle Wallet

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) recebidas por HTTP e SQS. As decisões de projeto e os endpoints estão em [ARCHITECTURE.md](ARCHITECTURE.md).

## Pré-requisitos

- Docker com Docker Compose v2
- Go 1.27.1 (apenas para rodar os testes fora do Docker)

## Subir o ambiente

```bash
docker compose up --build
```

Sobe PostgreSQL, Keycloak, LocalStack (SQS), aplica as migrations e inicia três instâncias da API.

| Serviço | Endereço |
|---|---|
| API | http://localhost:8080, http://localhost:8082, http://localhost:8083 |
| Swagger | http://localhost:8080/docs |
| Keycloak | http://localhost:8081 (admin: `admin` / `admin`) |
| LocalStack (SQS) | http://localhost:4566 |
| PostgreSQL | `localhost:5433`, usuário/senha/banco `apostas` |

Verificar: `curl http://localhost:8080/health/ready`

Parar: `docker compose down` (use `-v` para apagar os dados).

## Credenciais (ambiente local)

| client_id | client_secret | Permissão |
|---|---|---|
| `wallet-internal` | `wallet-internal-secret` | abrir, consultar e reconciliar carteiras |
| `provider-a` | `provider-a-secret` | operações do provedor `provider-a` |
| `provider-b` | `provider-b-secret` | operações do provedor `provider-b` |
| `untrusted-client` | `untrusted-client-secret` | nenhuma |

No Swagger, clique em **Authorize** e informe `client_id` e `client_secret`. Também há uma collection do Postman em `postman/jungle-wallet.postman_collection.json`.

## Fluxo de requisições (curl, bash / Git Bash)

1. Obter os tokens no Keycloak:

```bash
token() {
  curl -s -X POST http://localhost:8081/realms/wallet/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" \
    | sed -E 's/.*"access_token":"([^"]+)".*/\1/'
}
INTERNAL=$(token wallet-internal)
PROVIDER_A=$(token provider-a)
PLAYER=0192f28f-5dc0-7d58-bdb2-6a9c8e0f1234
```

2. Abrir a carteira (serviço interno):

```bash
curl -s -X POST http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}"
# -> 201 {"id":"<walletId>", ... "balance":{"amount":"100.00","currency":"BRL"}, "version":1}

WALLET=<walletId da resposta>
```

3. Enviar uma aposta por HTTP (provedor). Repetir o comando com o mesmo `$EXT` devolve `200` com `idempotentReplay: true`; a mesma chave com outro conteúdo devolve `409`.

```bash
EXT=transaction-$(date +%s)
curl -s -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:$EXT" \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$EXT\",
       \"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",
       \"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
```

Valores monetários são sempre strings decimais (`"25.00"`); um número JSON (`25.00`) é recusado com `400`.

4. Enviar uma aposta pelo SQS. O token do provedor vai no atributo de mensagem `Authorization`:

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

5. Ler os eventos publicados:

```bash
docker compose exec -T localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10
```

## Testes

Com o ambiente no ar:

```bash
export TEST_DATABASE_URL="postgres://apostas:apostas@localhost:5433/postgres?sslmode=disable"
export TEST_KEYCLOAK_URL="http://localhost:8081"
export TEST_SQS_ENDPOINT="http://localhost:4566"
export TEST_API_URLS="http://localhost:8080,http://localhost:8082,http://localhost:8083"

go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

Sem essas variáveis, os testes de integração são pulados.

No Windows, `go test -race` exige um compilador C. Sem ele, rode pelo container Go (Git Bash):

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$(pwd -W):/src" -w /src \
  -e TEST_DATABASE_URL="postgres://apostas:apostas@host.docker.internal:5433/postgres?sslmode=disable" \
  -e TEST_KEYCLOAK_URL="http://host.docker.internal:8081" \
  -e TEST_SQS_ENDPOINT="http://host.docker.internal:4566" \
  -e TEST_API_URLS="http://host.docker.internal:8080,http://host.docker.internal:8082,http://host.docker.internal:8083" \
  golang:1.27.1 go test -race ./...
```

## Configuração

As variáveis de ambiente, com valores locais de exemplo, estão em [.env.example](.env.example).
