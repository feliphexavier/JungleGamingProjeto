package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Scale é o número fixo de casas decimais de todos os valores monetários.
const Scale = 2

var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// Currency é um código de moeda no formato ISO 4217 (três letras maiúsculas).
// O valor zero é inválido.
type Currency struct {
	code string
}

// ParseCurrency valida e cria uma Currency.
func ParseCurrency(code string) (Currency, error) {
	if !currencyPattern.MatchString(code) {
		return Currency{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, code)
	}
	return Currency{code: code}, nil
}

// MustCurrency é ParseCurrency para constantes conhecidas; entra em pânico
// apenas por erro de programação, nunca por entrada externa.
func MustCurrency(code string) Currency {
	c, err := ParseCurrency(code)
	if err != nil {
		panic(err)
	}
	return c
}

// BRL é a moeda dos cenários principais.
var BRL = MustCurrency("BRL")

func (c Currency) String() string { return c.code }

// IsZero informa se a moeda não foi inicializada.
func (c Currency) IsZero() bool { return c.code == "" }

// Money é um valor monetário imutável: inteiro em unidades mínimas (centavos,
// com escala fixa de 2 casas) mais a moeda. Nunca passa por ponto flutuante.
//
// Limites: de -92233720368547758.08 a 92233720368547758.07 (limites do int64
// em centavos). Operações que ultrapassem esses limites retornam ErrAmountOverflow.
//
// O valor zero de Money (sem moeda) é inválido e é rejeitado pelas operações.
type Money struct {
	minor    int64
	currency Currency
}

// externalAmountPattern é o formato aceito em entradas financeiras externas:
// sem sinal, sem zeros à esquerda, exatamente duas casas decimais. Por ser uma
// forma única, não há normalização antes do hash de idempotência.
var externalAmountPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.[0-9]{2}$`)

// ParseMoney interpreta uma entrada financeira externa, como
// {"amount":"25.00","currency":"BRL"}. Rejeita vazio, NaN, Infinity, notação
// científica, sinal, zeros à esquerda e escala diferente de duas casas, sem
// arredondar.
func ParseMoney(amount, currency string) (Money, error) {
	cur, err := ParseCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	if strings.HasPrefix(amount, "-") {
		return Money{}, fmt.Errorf("%w: %q", ErrNegativeAmount, amount)
	}
	if !externalAmountPattern.MatchString(amount) {
		return Money{}, fmt.Errorf("%w: %q (esperado formato 0.00)", ErrInvalidAmount, amount)
	}

	// "25.00" vira "2500": o inteiro em centavos é exatamente a string sem o
	// ponto. strconv.ParseInt detecta overflow do int64.
	digits := strings.Replace(amount, ".", "", 1)
	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrAmountOverflow, amount)
	}
	return Money{minor: minor, currency: cur}, nil
}

// NewMoneyFromMinor cria um Money a partir de unidades mínimas, por exemplo ao
// reidratar valores do banco. Aceita negativos, usados em diferenças internas.
func NewMoneyFromMinor(minor int64, currency Currency) (Money, error) {
	if currency.IsZero() {
		return Money{}, fmt.Errorf("%w: currency", ErrUninitialized)
	}
	return Money{minor: minor, currency: currency}, nil
}

// Zero devolve o valor zero na moeda informada.
func Zero(currency Currency) (Money, error) {
	return NewMoneyFromMinor(0, currency)
}

// Minor devolve o valor em unidades mínimas.
func (m Money) Minor() int64 { return m.minor }

// Currency devolve a moeda.
func (m Money) Currency() Currency { return m.currency }

// IsValid informa se o Money foi inicializado.
func (m Money) IsValid() bool { return !m.currency.IsZero() }

func (m Money) IsZero() bool     { return m.minor == 0 }
func (m Money) IsPositive() bool { return m.minor > 0 }
func (m Money) IsNegative() bool { return m.minor < 0 }

// Add soma dois valores da mesma moeda.
func (m Money) Add(other Money) (Money, error) {
	if err := m.checkCompatible(other); err != nil {
		return Money{}, err
	}
	if (other.minor > 0 && m.minor > math.MaxInt64-other.minor) ||
		(other.minor < 0 && m.minor < math.MinInt64-other.minor) {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrAmountOverflow, m, other)
	}
	return Money{minor: m.minor + other.minor, currency: m.currency}, nil
}

// Sub subtrai other de m. O resultado pode ser negativo.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.checkCompatible(other); err != nil {
		return Money{}, err
	}
	if (other.minor < 0 && m.minor > math.MaxInt64+other.minor) ||
		(other.minor > 0 && m.minor < math.MinInt64+other.minor) {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrAmountOverflow, m, other)
	}
	return Money{minor: m.minor - other.minor, currency: m.currency}, nil
}

// Neg devolve o valor com sinal invertido.
func (m Money) Neg() (Money, error) {
	if !m.IsValid() {
		return Money{}, fmt.Errorf("%w: money", ErrUninitialized)
	}
	if m.minor == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: -(%s)", ErrAmountOverflow, m)
	}
	return Money{minor: -m.minor, currency: m.currency}, nil
}

// Cmp compara dois valores da mesma moeda: -1 se m < other, 0 se iguais,
// +1 se m > other.
func (m Money) Cmp(other Money) (int, error) {
	if err := m.checkCompatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.minor < other.minor:
		return -1, nil
	case m.minor > other.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal informa se os dois valores têm o mesmo valor e a mesma moeda.
func (m Money) Equal(other Money) bool {
	return m.minor == other.minor && m.currency == other.currency
}

func (m Money) checkCompatible(other Money) error {
	if !m.IsValid() || !other.IsValid() {
		return fmt.Errorf("%w: money", ErrUninitialized)
	}
	if m.currency != other.currency {
		return fmt.Errorf("%w: %s and %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	return nil
}

// Amount devolve o valor como string decimal com duas casas, por exemplo
// "25.00" ou "-5.00".
func (m Money) Amount() string {
	s := strconv.FormatInt(m.minor, 10)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	if len(s) <= Scale {
		s = strings.Repeat("0", Scale+1-len(s)) + s
	}
	return sign + s[:len(s)-Scale] + "." + s[len(s)-Scale:]
}

// String devolve o valor legível, por exemplo "25.00 BRL".
func (m Money) String() string {
	if !m.IsValid() {
		return "<invalid money>"
	}
	return m.Amount() + " " + m.currency.code
}

type moneyJSON struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// MarshalJSON serializa no contrato externo {"amount":"25.00","currency":"BRL"}.
// Valores negativos (diferenças internas) saem com sinal, por exemplo "-5.00".
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, fmt.Errorf("%w: money", ErrUninitialized)
	}
	return json.Marshal(moneyJSON{Amount: m.Amount(), Currency: m.currency.code})
}

// UnmarshalJSON lê o contrato externo com as mesmas regras de ParseMoney:
// o amount precisa ser uma string não negativa com duas casas. Um número JSON
// (25.00 sem aspas) é rejeitado, pois seria decodificado como float.
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw struct {
		Amount   json.RawMessage `json:"amount"`
		Currency string          `json:"currency"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAmount, err)
	}
	var amount string
	if err := json.Unmarshal(raw.Amount, &amount); err != nil {
		return fmt.Errorf("%w: amount deve ser string decimal", ErrInvalidAmount)
	}
	parsed, err := ParseMoney(amount, raw.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
