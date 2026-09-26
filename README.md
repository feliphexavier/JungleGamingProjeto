# Jungle Wallet

Serviço em Go que processa operações financeiras de provedores de jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) recebidas por HTTP e SQS. As decisões de projeto e os endpoints estão em [ARCHITECTURE.md](ARCHITECTURE.md).

## Início rápido: reproduzir no Postman

1. Clone o repositório e suba o ambiente (detalhes e pré-requisitos nas seções [1](#1-clonar) e [2](#2-subir-o-ambiente)):

   ```bash
   git clone https://github.com/feliphexavier/JungleGamingProjeto.git
   cd JungleGamingProjeto
   docker compose up --build -d --wait
   ```

   Espere o comando terminar (a primeira vez leva de 5 a 10 minutos).

2. No Postman: **Import** → selecione o arquivo `postman/jungle-wallet.postman_collection.json`. Não é preciso criar environment nem configurar autenticação: as URLs já estão nas variáveis da collection, e os tokens, o `playerId`, o `walletId` e os ids das transações são gerados e salvos automaticamente pelos scripts das requisições.

3. Execute as pastas **na ordem**, de cima para baixo:

   | Pasta | O que demonstra |
   |---|---|
   | 0. Health | ambiente no ar |
   | 1. Tokens (Keycloak) | obtém os tokens de cada client (rode antes das demais) |
   | 2. Carteira | abre uma carteira com `100.00`, consulta saldo, ledger e reconciliação |
   | 3. Operações do provedor (HTTP) | `BET`, replay idempotente, conflito `409`, `WIN`, `REFUND`, saldo insuficiente `422` |
   | 4. Segurança | `401`, `403` e `404` para tokens ausentes, adulterados ou de outro provedor |
   | 5. SQS (LocalStack) | envia operações pela fila, deduplicação, DLQ e leitura dos eventos publicados |

   O jeito mais simples é rodar tudo de uma vez: clique com o botão direito na collection → **Run collection** → **Run**. Todas as requisições têm testes; o resultado esperado é tudo verde.

4. Para repetir do zero, basta rodar a collection de novo: cada execução cria um jogador e uma carteira novos. Os tokens expiram em 5 minutos; se aparecer `401` inesperado, rode de novo a pasta **1. Tokens**.

## Pré-requisitos

| Ferramenta | Para quê |
|---|---|
| Git | clonar o repositório |
| Docker com Docker Compose v2 (Docker Desktop no Windows/macOS, **em execução**) | subir o ambiente |
| Um terminal bash: Linux/macOS nativo, ou **Git Bash** no Windows | todos os comandos deste README (não funcionam no PowerShell/cmd) |
| `curl` | chamar a API (já vem com o Git Bash) |
| Go 1.27.1 | apenas para rodar os testes fora do Docker |

Portas que precisam estar livres no host: `8080`, `8081`, `8082`, `8083`, `4566` e `5433`.

## 1. Clonar

```bash
git clone https://github.com/feliphexavier/JungleGamingProjeto.git
cd JungleGamingProjeto
```

Nenhum arquivo `.env` é necessário para subir pelo Docker: toda a configuração já está no `docker-compose.yml`.

## 2. Subir o ambiente

```bash
docker compose up --build -d --wait
```

Sobe PostgreSQL, Keycloak, LocalStack (SQS), aplica as migrations e inicia três instâncias da API. O comando só devolve o terminal quando tudo estiver saudável.

**A primeira execução é lenta (5 a 10 minutos)**: baixa as imagens, compila a aplicação e o Keycloak leva de 1 a 3 minutos para iniciar. As próximas execuções levam segundos.

Verificar:

```bash
curl http://localhost:8080/health/ready
# -> {"checks":{"postgres":"UP","sqs":"UP"},"status":"UP"}
```

| Serviço | Endereço |
|---|---|
| API | http://localhost:8080, http://localhost:8082, http://localhost:8083 |
| Swagger | http://localhost:8080/docs |
| Keycloak | http://127.0.0.1:8081 (admin: `admin` / `admin`) |
| LocalStack (SQS) | http://localhost:4566 |
| PostgreSQL | `localhost:5433`, usuário/senha/banco `apostas` |

Logs: `docker compose logs -f api`. Parar: `docker compose down` (use `-v` para apagar também os dados).

## 3. Credenciais (ambiente local)

| client_id | client_secret | Permissão |
|---|---|---|
| `wallet-internal` | `wallet-internal-secret` | abrir, consultar e reconciliar carteiras |
| `provider-a` | `provider-a-secret` | operações do provedor `provider-a` |
| `provider-b` | `provider-b-secret` | operações do provedor `provider-b` |
| `untrusted-client` | `untrusted-client-secret` | nenhuma |

No Swagger, clique em **Authorize** e informe `client_id` e `client_secret`. Também há uma collection do Postman em `postman/jungle-wallet.postman_collection.json`.

## 4. Fluxo de requisições (bash / Git Bash)

Rode os blocos em sequência **no mesmo terminal**: cada um usa as variáveis definidas nos anteriores.

1. Obter os tokens no Keycloak. Os tokens expiram em 5 minutos; se alguma chamada devolver `401`, rode este bloco de novo.

```bash
token() {
  curl -s -X POST http://127.0.0.1:8081/realms/wallet/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" \
    | sed -E 's/.*"access_token":"([^"]+)".*/\1/'
}
INTERNAL=$(token wallet-internal)
PROVIDER_A=$(token provider-a)
PLAYER=0192f28f-5dc0-7d58-bdb2-6a9c8e0f1234
```

2. Abrir a carteira (serviço interno) e guardar o `walletId`:

```bash
RESP=$(curl -s -X POST http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")
echo "$RESP"
# -> {"id":"<walletId>", ... "balance":{"amount":"100.00","currency":"BRL"}, "version":1}

WALLET=$(echo "$RESP" | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "$WALLET"
```

Cada jogador tem uma única carteira: rodar este bloco de novo com o mesmo `PLAYER` devolve `409`. Para abrir outra, troque o `PLAYER` por outro UUID.

3. Enviar uma aposta por HTTP (provedor). Repetir o comando com o mesmo `$EXT` devolve `200` com `idempotentReplay: true`; a mesma chave com outro conteúdo devolve `409`.

```bash
EXT=transaction-$(date +%s)
curl -s -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: provider-a:$EXT" \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$EXT\",
       \"playerId\":\"$PLAYER\",\"walletId\":\"$WALLET\",\"roundId\":\"round-987\",
       \"gameId\":\"fortune-chimp\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"
# -> {"transactionId":"...","status":"PROCESSED","balance":{"amount":"75.00","currency":"BRL"},"idempotentReplay":false}
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

(`MSYS_NO_PATHCONV=1` só é necessário no Git Bash do Windows; nos demais é ignorado.)

5. Conferir o saldo (deve ser `65.00` depois das duas apostas):

```bash
curl -s http://localhost:8080/wallets/$WALLET -H "Authorization: Bearer $INTERNAL"
```

6. Ler os eventos publicados:

```bash
docker compose exec -T localstack awslocal sqs receive-message \
  --queue-url http://localhost:4566/000000000000/wallet-events.fifo --max-number-of-messages 10
```

## 5. Testes

Com o ambiente no ar (passo 2), na raiz do repositório:

```bash
export TEST_DATABASE_URL="postgres://apostas:apostas@localhost:5433/postgres?sslmode=disable"
export TEST_KEYCLOAK_URL="http://127.0.0.1:8081"
export TEST_SQS_ENDPOINT="http://localhost:4566"
export TEST_API_URLS="http://localhost:8080,http://localhost:8082,http://localhost:8083"

go test ./...
go vet ./...
gofmt -l .
```

Sem essas variáveis, os testes de integração são pulados. A suíte completa leva cerca de 1 minuto: são testes de integração contra o PostgreSQL, o Keycloak e o SQS reais, e alguns esperam de propósito pelos ciclos de leitura da fila.

## Problemas comuns

| Sintoma | Causa e solução |
|---|---|
| `Cannot connect to the Docker daemon` / `error during connect` | O Docker Desktop não está aberto. Abra-o e espere ficar "running". |
| `port is already allocated` / `address already in use` | Outro processo usa uma das portas listadas nos pré-requisitos. Pare-o, ou rode `docker compose down` se for uma subida anterior deste projeto. |
| `dependency failed to start: container ... is unhealthy` | Algum serviço demorou mais que o esperado (máquina lenta na primeira subida). Rode `docker compose up -d --wait` de novo; os containers já criados continuam de onde pararam. Para ver o motivo: `docker compose logs keycloak` (ou `localstack`, `postgres`, `migrate`). |
| `401` nas chamadas | Token expirado (5 minutos). Rode de novo o bloco 1 do fluxo. |
| `409` ao abrir carteira | Já existe carteira para esse `PLAYER`. Use outro UUID. |
| `gofmt -l .` lista arquivos no Windows | O clone foi feito antes do `.gitattributes` forçar LF. Rode `git rm --cached -rq . && git reset --hard` (descarta alterações locais não commitadas). |
| Quero recomeçar do zero | `docker compose down -v` e depois o passo 2. |

## Configuração

As variáveis de ambiente, com valores locais de exemplo, estão em [.env.example](.env.example). Só são necessárias para rodar a API fora do Docker, apontando para os serviços do Compose. A aplicação lê variáveis de ambiente, não o arquivo; carregue-o no shell antes (troque `HTTP_PORT` se a API do Compose estiver ocupando a `8080`):

```bash
set -a; . ./.env.example; set +a
HTTP_PORT=9090 go run ./cmd/api
```
