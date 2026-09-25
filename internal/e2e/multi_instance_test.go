// Package e2e testa a stack do Docker Compose de fora, como caixa-preta: três
// instâncias independentes da API (processos e containers separados), o
// Keycloak real, o PostgreSQL e o SQS do LocalStack.
//
// Variáveis (sem TEST_API_URLS o teste é pulado):
//   - TEST_API_URLS: URLs das instâncias separadas por vírgula
//     (ex.: http://localhost:8080,http://localhost:8082,http://localhost:8083)
//   - TEST_KEYCLOAK_URL: ex.: http://localhost:8081
//   - TEST_SQS_ENDPOINT: ex.: http://localhost:4566
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"

	sqsinfra "github.com/feliphexavier/jungleGamingProjeto/internal/infra/sqs"
	"github.com/feliphexavier/jungleGamingProjeto/internal/testsupport/kctest"
)

type stack struct {
	t         *testing.T
	apis      []string
	internal  string
	providerA string
	kc        *kctest.Keycloak
}

func newStack(t *testing.T) *stack {
	t.Helper()
	raw := os.Getenv("TEST_API_URLS")
	if raw == "" {
		t.Skip("TEST_API_URLS não definida: teste com várias instâncias pulado")
	}
	apis := strings.Split(raw, ",")
	if len(apis) < 3 {
		t.Fatalf("TEST_API_URLS deve ter ao menos 3 instâncias, tem %d", len(apis))
	}
	kc := kctest.New(t)
	s := &stack{t: t, apis: apis, kc: kc,
		internal:  kc.Token(t, "wallet-internal", "wallet-internal-secret"),
		providerA: kc.Token(t, "provider-a", "provider-a-secret"),
	}
	for _, api := range apis {
		if r := s.call(api, "GET", "/health/ready", "", nil, nil); r.status != http.StatusOK {
			t.Fatalf("instância %s não está pronta: %d %v", api, r.status, r.body)
		}
	}
	return s
}

type result struct {
	status int
	body   map[string]any
}

var client = &http.Client{Timeout: 30 * time.Second}

func (s *stack) call(api, method, path, token string, body any, headers map[string]string) result {
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, api+path, reader)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		s.t.Errorf("%s %s%s: %v", method, api, path, err)
		return result{}
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return result{status: res.StatusCode, body: out}
}

func money(amount string) map[string]string {
	return map[string]string{"amount": amount, "currency": "BRL"}
}

type wallet struct{ id, player string }

func (s *stack) openWallet(amount string) wallet {
	s.t.Helper()
	player := uuid.NewString()
	r := s.call(s.apis[0], "POST", "/wallets", s.internal, map[string]any{"playerId": player, "initialBalance": money(amount)}, nil)
	if r.status != http.StatusCreated {
		s.t.Fatalf("abrir carteira: %d %v", r.status, r.body)
	}
	return wallet{id: r.body["id"].(string), player: player}
}

func (s *stack) bet(api string, w wallet, externalID, amount string) result {
	return s.call(api, "POST", "/wagering/transactions", s.providerA, map[string]any{
		"providerId": "provider-a", "externalTransactionId": externalID,
		"playerId": w.player, "walletId": w.id, "roundId": "round-1", "gameId": "fortune-chimp",
		"kind": "BET", "money": money(amount),
	}, map[string]string{"Idempotency-Key": "provider-a:" + externalID})
}

// check confere o saldo em todas as instâncias, a reconciliação com o ledger
// e a quantidade de lançamentos.
func (s *stack) check(w wallet, balance string, entries int) {
	s.t.Helper()
	for _, api := range s.apis {
		r := s.call(api, "GET", "/wallets/"+w.id, s.internal, nil, nil)
		if got := r.body["balance"].(map[string]any)["amount"]; got != balance {
			s.t.Errorf("%s: saldo = %v, want %s", api, got, balance)
		}
	}
	r := s.call(s.apis[1], "POST", "/wallets/"+w.id+"/reconciliation", s.internal, nil, nil)
	if r.body["consistent"] != true || int(r.body["checkedEntries"].(float64)) != entries {
		s.t.Errorf("reconciliação = %v, want consistente com %d lançamentos", r.body, entries)
	}
}

// Duas apostas de 80.00 sobre 100.00, cada uma em uma instância diferente, ao
// mesmo tempo. Repetido em 20 carteiras para exercitar a disputa.
func TestConcurrentBetsAcrossInstances(t *testing.T) {
	s := newStack(t)
	for round := 0; round < 20; round++ {
		w := s.openWallet("100.00")
		var (
			wg      sync.WaitGroup
			results [2]result
			start   = make(chan struct{})
		)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = s.bet(s.apis[i], w, fmt.Sprintf("bet-%s-%d", w.id, i), "80.00")
			}()
		}
		close(start)
		wg.Wait()

		statuses := []int{results[0].status, results[1].status}
		slices.Sort(statuses)
		if statuses[0] != http.StatusCreated || statuses[1] != http.StatusUnprocessableEntity {
			t.Fatalf("carteira %s: status = %v, want [201 422] (%v / %v)", w.id, statuses, results[0].body, results[1].body)
		}
		s.check(w, "20.00", 2) // abertura + um débito
	}
}

// A mesma aposta enviada 50 vezes em paralelo, distribuída entre as três
// instâncias: um débito, 49 replays com o mesmo resultado.
func TestSameBet50TimesAcrossInstances(t *testing.T) {
	s := newStack(t)
	w := s.openWallet("100.00")
	ext := "bet-" + w.id

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		codes   = map[int]int{}
		txIDs   = map[any]bool{}
		start   = make(chan struct{})
		balance = map[any]bool{}
	)
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := s.bet(s.apis[i%len(s.apis)], w, ext, "10.00")
			mu.Lock()
			defer mu.Unlock()
			codes[r.status]++
			txIDs[r.body["transactionId"]] = true
			if b, ok := r.body["balance"].(map[string]any); ok {
				balance[b["amount"]] = true
			}
		}()
	}
	close(start)
	wg.Wait()

	if codes[http.StatusCreated] != 1 || codes[http.StatusOK] != 49 {
		t.Errorf("status = %v, want 1×201 e 49×200", codes)
	}
	if len(txIDs) != 1 || len(balance) != 1 || !balance["90.00"] {
		t.Errorf("respostas divergentes: transações %v, saldos %v", txIDs, balance)
	}
	s.check(w, "90.00", 2)
}

// Operações enviadas pelo SQS e consumidas pelas três instâncias ao mesmo
// tempo, com entregas duplicadas. Depois, os eventos publicados pelas três
// outboxes chegam completos e na ordem de cada carteira.
func TestSQSAndOutboxAcrossInstances(t *testing.T) {
	s := newStack(t)
	endpoint := os.Getenv("TEST_SQS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_SQS_ENDPOINT não definida")
	}
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Setenv("AWS_ACCESS_KEY_ID", "test")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	}
	ctx := context.Background()
	cfg := sqsinfra.Config{Endpoint: endpoint, Region: "us-east-1",
		InboundQueue: "wager-operations.fifo", DLQ: "wager-operations-dlq.fifo", EventsQueue: "wallet-events.fifo"}
	sq, err := sqsinfra.NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	queues := &sqsinfra.Queues{}
	if err := queues.Resolve(ctx, sq, cfg); err != nil {
		t.Fatal(err)
	}

	// 6 carteiras × 5 apostas de 1.00, cada mensagem enviada duas vezes.
	const perWallet = 5
	wallets := make([]wallet, 6)
	for i := range wallets {
		wallets[i] = s.openWallet("100.00")
	}
	for _, w := range wallets {
		for j := range perWallet {
			id := fmt.Sprintf("sqs-%s-%d", w.id, j)
			body, _ := json.Marshal(map[string]any{
				"messageId": "msg-" + id, "type": sqsinfra.MessageType, "occurredAt": time.Now().UTC(),
				"data": map[string]any{
					"idempotencyKey": "provider-a:" + id, "providerId": "provider-a", "externalTransactionId": id,
					"playerId": w.player, "walletId": w.id, "roundId": "r", "gameId": "g",
					"kind": "BET", "money": money("1.00"),
				},
			})
			for _, dup := range []string{"a", "b"} {
				_, err := sq.SendMessage(ctx, &sqs.SendMessageInput{
					QueueUrl: aws.String(queues.Inbound), MessageBody: aws.String(string(body)),
					MessageGroupId: aws.String(w.id), MessageDeduplicationId: aws.String(id + "-" + dup),
					MessageAttributes: map[string]types.MessageAttributeValue{
						sqsinfra.AuthorizationAttribute: {DataType: aws.String("String"), StringValue: aws.String("Bearer " + s.providerA)},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	// Espera o consumo: saldo 95.00 em todas as carteiras.
	deadline := time.Now().Add(60 * time.Second)
	for _, w := range wallets {
		for {
			r := s.call(s.apis[2], "GET", "/wallets/"+w.id, s.internal, nil, nil)
			if r.body["balance"].(map[string]any)["amount"] == "95.00" || time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		s.check(w, "95.00", 1+perWallet)
	}

	// Eventos: para cada carteira, WalletBalanceChanged com walletVersion
	// 1..6 exatamente nessa ordem (reenvios com o mesmo eventId são ignorados).
	want := map[string]bool{}
	for _, w := range wallets {
		want[w.id] = true
	}
	versions := map[string][]int64{}
	seen := map[string]bool{}
	deadline = time.Now().Add(60 * time.Second)
	complete := func() bool {
		for id := range want {
			if len(versions[id]) < 1+perWallet {
				return false
			}
		}
		return true
	}
	for !complete() && time.Now().Before(deadline) {
		out, err := sq.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl: aws.String(queues.Events), MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range out.Messages {
			var env struct {
				EventID   string `json:"eventId"`
				EventType string `json:"eventType"`
				Data      struct {
					WalletID      string `json:"walletId"`
					WalletVersion int64  `json:"walletVersion"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &env)
			_, _ = sq.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(queues.Events), ReceiptHandle: m.ReceiptHandle})
			if env.EventType != "WalletBalanceChanged" || !want[env.Data.WalletID] || seen[env.EventID] {
				continue
			}
			seen[env.EventID] = true
			versions[env.Data.WalletID] = append(versions[env.Data.WalletID], env.Data.WalletVersion)
		}
	}
	for id := range want {
		got := versions[id]
		ok := len(got) == 1+perWallet
		for i, v := range got {
			ok = ok && v == int64(i+1)
		}
		if !ok {
			t.Errorf("carteira %s: walletVersion dos eventos = %v, want 1..%d em ordem", id, got, 1+perWallet)
		}
	}
}
