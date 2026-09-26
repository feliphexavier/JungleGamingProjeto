# Principais decisões arquiteturais

1. Foi decidido estruturar o projeto utilizando a arquitetura hexagonal (portas e adaptadores), tendo em vista a natureza compartimentada do projeto e o fato de haver dois canais de entrada que consomem uma única regra de negócio. Esses canais são HTTP e SQS, que produzem exatamente o mesmo resultado, dada a idempotência da requisição, pois chamam o mesmo caso de uso na aplicação.

   A estrutura de pastas foi organizada da seguinte forma:

```
.
├── api/
│   ├── openapi.yaml                 # especificação OpenAPI 3 (servida em /openapi.yaml e /docs)
│   └── embed.go                     # embute a especificação no binário
├── cmd/
│   ├── api/
│   │   └── main.go                  # entrada da aplicação: carrega a config e roda o Fx
│   └── migrate/
│       └── main.go                  # migrations com goose (up, down, status)
├── internal/
│   ├── domain/                      # regras de negócio, sem dependência de infraestrutura
│   │   ├── money.go                 # dinheiro em int64 (centavos), parsing de string decimal
│   │   ├── wallet.go                # carteira: saldo, versão, débito e crédito
│   │   ├── transaction.go           # transação de aposta e seus estados
│   │   ├── process.go               # regras de BET, WIN, LOSS, REFUND, ROLLBACK
│   │   ├── ledger.go                # lançamentos do ledger
│   │   ├── events.go                # eventos de integração e envelope
│   │   ├── idempotency.go           # hash canônico do payload
│   │   ├── failure.go, errors.go    # códigos de rejeição e erros de domínio
│   │   └── *_test.go                # testes de unidade
│   ├── app/                         # casos de uso; depende só de interfaces
│   │   ├── ports.go                 # Store, repositórios, EventPublisher, Clock
│   │   ├── wallet.go                # abrir carteira
│   │   ├── wager.go                 # SubmitWager (HTTP e SQS)
│   │   ├── pending.go               # retomada de operações aguardando referência
│   │   ├── queries.go               # consultas, ledger paginado, reconciliação
│   │   ├── outbox.go                # relay da transactional outbox
│   │   ├── service.go, errors.go
│   │   └── *_test.go                # integração com PostgreSQL real
│   ├── infra/
│   │   ├── postgres/                # pgx com SQL explícito, locks e outbox/inbox
│   │   ├── httpapi/                 # rotas chi, autenticação, erros, DTOs
│   │   ├── auth/                    # validação de JWT (OIDC/JWKS)
│   │   │   └── authtest/            # emissor local para testes de tokens inválidos
│   │   └── sqs/                     # cliente, consumidor e publicador SQS
│   ├── bootstrap/
│   │   └── modules.go               # módulos Fx e ciclo de vida (início e shutdown)
│   ├── config/
│   │   └── config.go                # leitura e validação das variáveis de ambiente
│   ├── worker/
│   │   └── loop.go                  # laço de worker com encerramento observável
│   ├── e2e/
│   │   └── multi_instance_test.go   # testes contra as 3 instâncias do Compose
│   └── testsupport/                 # banco, filas e tokens descartáveis para testes
│       ├── pgtest/
│       ├── sqstest/
│       └── kctest/
├── migrations/
│   ├── 20260925165927_init_schema.sql
│   ├── 20260925200000_outbox_publish_order.sql
│   └── embed.go                     # migrations embutidas no binário
├── deploy/
│   ├── keycloak/
│   │   └── wallet-realm.json        # realm, clients e papéis importados na subida
│   └── localstack/
│       └── init-queues.sh           # criação das filas SQS
├── postman/
│   └── jungle-wallet.postman_collection.json
├── docker-compose.yml               # postgres, keycloak, localstack, migrate, 3 instâncias da API
├── Dockerfile                       # build em dois estágios, runtime distroless não root
├── Makefile                         # atalhos opcionais
├── .env.example                     # variáveis de configuração com valores locais
├── ARCHITECTURE.md                  # decisões de projeto
└── README.md
```

2. Garantia do cumprimento das regras de negócio por meio de triggers e constraints no SQL, além da validação via código, garantindo assim que não haja saldo negativo, mutabilidade nem transações duplicadas.

3. Dinheiro representado por int64/BIGINT, dada a imprecisão e a propagação de erro ao usar valores do tipo float (ex.: 0.1 + 0.2 = 0.30000000000000004), o que, a longo prazo, poderia causar prejuízos à instituição financeira. Além disso, a conversão é feita diretamente de string para int, sem passar por float, para que não sejam aceitos NaN, números em notação científica nem valores negativos.

4. Bloqueio das instruções UPDATE/DELETE no ledger, dada a natureza da feature.

5. Utilização de FOR UPDATE no SQL para que os valores sejam lidos antes de realizar uma atualização, mesmo com várias instâncias.

6. A idempotência fica persistida como chave do cliente mais hash do payload, e é esse mecanismo que garante a consistência entre os dois canais, HTTP e SQS.

7. O providerId é um claim fixo do Keycloak e não vem no corpo da requisição, fazendo com que a transação de outro provedor apareça como inexistente.

## Endpoints

| Rota | Quem pode acessar |
|---|---|
| `POST /wallets` | serviço interno |
| `GET /wallets/{walletId}` | serviço interno |
| `GET /wallets/{walletId}/ledger?cursor=&limit=` | serviço interno |
| `POST /wallets/{walletId}/reconciliation` | serviço interno |
| `POST /wagering/transactions` (header `Idempotency-Key`) | provedor |
| `GET /wagering/transactions/{transactionId}` | provedor (somente as próprias) ou serviço interno |
| `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` | o próprio provedor ou serviço interno |
| `GET /health/live`, `GET /health/ready` | público |
| `GET /docs`, `GET /openapi.yaml` | público (documentação) |

Respostas de `POST /wagering/transactions`: `201` processada, `200` replay, `202` aguardando a transação de referência, `422` rejeitada por regra de negócio (ex.: saldo insuficiente).

Erros: `400` entrada inválida, `401` sem autenticação válida, `403` sem permissão, `404` não encontrado, `409` conflito de idempotência, `503` indisponibilidade transitória (com `Retry-After`).
