package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

var usd = MustCurrency("USD")

func mustMoney(t *testing.T, amount, currency string) Money {
	t.Helper()
	m, err := ParseMoney(amount, currency)
	if err != nil {
		t.Fatalf("ParseMoney(%q, %q): %v", amount, currency, err)
	}
	return m
}

func mustMinor(t *testing.T, minor int64, c Currency) Money {
	t.Helper()
	m, err := NewMoneyFromMinor(minor, c)
	if err != nil {
		t.Fatalf("NewMoneyFromMinor(%d): %v", minor, err)
	}
	return m
}

func TestParseCurrency(t *testing.T) {
	valid := []string{"BRL", "USD", "EUR"}
	for _, code := range valid {
		if _, err := ParseCurrency(code); err != nil {
			t.Errorf("ParseCurrency(%q) = %v, want nil", code, err)
		}
	}

	invalid := []string{"", "brl", "BR", "BRLL", "B1L", " BRL", "BRL "}
	for _, code := range invalid {
		if _, err := ParseCurrency(code); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) = %v, want ErrInvalidCurrency", code, err)
		}
	}
}

func TestParseMoneyValid(t *testing.T) {
	tests := []struct {
		amount string
		minor  int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"0.10", 10},
		{"1.00", 100},
		{"25.00", 2500},
		{"975.50", 97550},
		{"1000.00", 100000},
		{"92233720368547758.07", math.MaxInt64},
	}
	for _, tt := range tests {
		m, err := ParseMoney(tt.amount, "BRL")
		if err != nil {
			t.Errorf("ParseMoney(%q) error: %v", tt.amount, err)
			continue
		}
		if m.Minor() != tt.minor {
			t.Errorf("ParseMoney(%q).Minor() = %d, want %d", tt.amount, m.Minor(), tt.minor)
		}
		if m.Currency() != BRL {
			t.Errorf("ParseMoney(%q).Currency() = %s, want BRL", tt.amount, m.Currency())
		}
		if got := m.Amount(); got != tt.amount {
			t.Errorf("ParseMoney(%q).Amount() = %q, want round-trip", tt.amount, got)
		}
	}
}

func TestParseMoneyInvalid(t *testing.T) {
	tests := []struct {
		name   string
		amount string
		want   error
	}{
		{"vazio", "", ErrInvalidAmount},
		{"espaço", " ", ErrInvalidAmount},
		{"NaN", "NaN", ErrInvalidAmount},
		{"Infinity", "Infinity", ErrInvalidAmount},
		{"+Infinity", "+Infinity", ErrInvalidAmount},
		{"notação científica", "1e3", ErrInvalidAmount},
		{"notação científica com casas", "2.5E+1", ErrInvalidAmount},
		{"sem casas", "25", ErrInvalidAmount},
		{"uma casa", "25.5", ErrInvalidAmount},
		{"três casas (escala excedente)", "25.001", ErrInvalidAmount},
		{"três casas terminando em zero", "25.000", ErrInvalidAmount},
		{"ponto sem inteiro", ".50", ErrInvalidAmount},
		{"ponto sem casas", "25.", ErrInvalidAmount},
		{"zero à esquerda", "025.00", ErrInvalidAmount},
		{"vírgula", "25,00", ErrInvalidAmount},
		{"separador de milhar", "1,000.00", ErrInvalidAmount},
		{"sinal positivo", "+25.00", ErrInvalidAmount},
		{"espaço em volta", " 25.00 ", ErrInvalidAmount},
		{"hexadecimal", "0x10.00", ErrInvalidAmount},
		{"negativo", "-25.00", ErrNegativeAmount},
		{"negativo zero", "-0.00", ErrNegativeAmount},
		{"overflow por um centavo", "92233720368547758.08", ErrAmountOverflow},
		{"overflow grande", "99999999999999999999.00", ErrAmountOverflow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMoney(tt.amount, "BRL")
			if !errors.Is(err, tt.want) {
				t.Errorf("ParseMoney(%q) = %v, want %v", tt.amount, err, tt.want)
			}
		})
	}
}

func TestParseMoneyInvalidCurrency(t *testing.T) {
	for _, c := range []string{"", "brl", "REAL"} {
		if _, err := ParseMoney("1.00", c); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseMoney currency %q = %v, want ErrInvalidCurrency", c, err)
		}
	}
}

func TestNewMoneyFromMinor(t *testing.T) {
	m := mustMinor(t, -500, BRL)
	if m.Amount() != "-5.00" {
		t.Errorf("Amount() = %q, want -5.00", m.Amount())
	}
	if _, err := NewMoneyFromMinor(100, Currency{}); !errors.Is(err, ErrUninitialized) {
		t.Errorf("moeda não inicializada = %v, want ErrUninitialized", err)
	}
}

func TestZero(t *testing.T) {
	z, err := Zero(BRL)
	if err != nil {
		t.Fatal(err)
	}
	if !z.IsZero() || z.Currency() != BRL || z.Amount() != "0.00" {
		t.Errorf("Zero(BRL) = %s", z)
	}
	if _, err := Zero(Currency{}); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Zero(Currency{}) = %v, want ErrUninitialized", err)
	}
}

func TestAmountFormatting(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{-1, "-0.01"},
		{99, "0.99"},
		{-99, "-0.99"},
		{100, "1.00"},
		{-2000, "-20.00"},
		{math.MaxInt64, "92233720368547758.07"},
		{math.MinInt64, "-92233720368547758.08"},
	}
	for _, tt := range tests {
		if got := mustMinor(t, tt.minor, BRL).Amount(); got != tt.want {
			t.Errorf("Amount(%d) = %q, want %q", tt.minor, got, tt.want)
		}
	}
}

func TestAddSub(t *testing.T) {
	a := mustMoney(t, "100.00", "BRL")
	b := mustMoney(t, "80.00", "BRL")

	sum, err := a.Add(b)
	if err != nil || sum.Amount() != "180.00" {
		t.Errorf("100.00 + 80.00 = %s, %v", sum, err)
	}

	diff, err := a.Sub(b)
	if err != nil || diff.Amount() != "20.00" {
		t.Errorf("100.00 - 80.00 = %s, %v", diff, err)
	}

	// Diferenças internas podem ser negativas.
	neg, err := b.Sub(a)
	if err != nil || neg.Amount() != "-20.00" || !neg.IsNegative() {
		t.Errorf("80.00 - 100.00 = %s, %v", neg, err)
	}

	// Imutabilidade: os operandos não mudam.
	if a.Amount() != "100.00" || b.Amount() != "80.00" {
		t.Errorf("operandos alterados: a=%s b=%s", a, b)
	}
}

func TestOverflow(t *testing.T) {
	maxM := mustMinor(t, math.MaxInt64, BRL)
	minM := mustMinor(t, math.MinInt64, BRL)
	one := mustMinor(t, 1, BRL)
	minusOne := mustMinor(t, -1, BRL)

	tests := []struct {
		name string
		op   func() (Money, error)
	}{
		{"max + 1", func() (Money, error) { return maxM.Add(one) }},
		{"min + (-1)", func() (Money, error) { return minM.Add(minusOne) }},
		{"min - 1", func() (Money, error) { return minM.Sub(one) }},
		{"max - (-1)", func() (Money, error) { return maxM.Sub(minusOne) }},
		{"0 - min", func() (Money, error) { return mustMinor(t, 0, BRL).Sub(minM) }},
		{"-(min)", func() (Money, error) { return minM.Neg() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.op(); !errors.Is(err, ErrAmountOverflow) {
				t.Errorf("err = %v, want ErrAmountOverflow", err)
			}
		})
	}

	// Nos limites exatos não há overflow.
	if got, err := maxM.Sub(one); err != nil || got.Minor() != math.MaxInt64-1 {
		t.Errorf("max - 1 = %v, %v", got, err)
	}
	if got, err := minM.Add(one); err != nil || got.Minor() != math.MinInt64+1 {
		t.Errorf("min + 1 = %v, %v", got, err)
	}
	if got, err := maxM.Neg(); err != nil || got.Minor() != -math.MaxInt64 {
		t.Errorf("-(max) = %v, %v", got, err)
	}
}

func TestNeg(t *testing.T) {
	m := mustMoney(t, "25.00", "BRL")
	n, err := m.Neg()
	if err != nil || n.Amount() != "-25.00" {
		t.Errorf("Neg(25.00) = %s, %v", n, err)
	}
	back, err := n.Neg()
	if err != nil || !back.Equal(m) {
		t.Errorf("Neg(Neg(25.00)) = %s, %v", back, err)
	}
	if _, err := (Money{}).Neg(); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Neg de Money zero-value = %v, want ErrUninitialized", err)
	}
}

func TestCmp(t *testing.T) {
	small := mustMoney(t, "20.00", "BRL")
	big := mustMoney(t, "80.00", "BRL")

	tests := []struct {
		a, b Money
		want int
	}{
		{small, big, -1},
		{big, small, 1},
		{big, mustMoney(t, "80.00", "BRL"), 0},
	}
	for _, tt := range tests {
		got, err := tt.a.Cmp(tt.b)
		if err != nil || got != tt.want {
			t.Errorf("Cmp(%s, %s) = %d, %v; want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestCurrencyMismatch(t *testing.T) {
	brl := mustMoney(t, "10.00", "BRL")
	dollars := mustMinor(t, 1000, usd)

	if _, err := brl.Add(dollars); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Sub(dollars); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Cmp(dollars); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp = %v, want ErrCurrencyMismatch", err)
	}
	if brl.Equal(dollars) {
		t.Error("Equal entre moedas diferentes com mesmo valor deveria ser false")
	}
}

func TestUninitializedMoneyRejected(t *testing.T) {
	var zero Money
	valid := mustMoney(t, "1.00", "BRL")

	if zero.IsValid() {
		t.Error("Money{} não deveria ser válido")
	}
	if _, err := zero.Add(valid); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Money{}.Add = %v, want ErrUninitialized", err)
	}
	if _, err := valid.Sub(zero); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Sub(Money{}) = %v, want ErrUninitialized", err)
	}
	if _, err := valid.Cmp(zero); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Cmp(Money{}) = %v, want ErrUninitialized", err)
	}
	if _, err := json.Marshal(zero); !errors.Is(err, ErrUninitialized) {
		t.Errorf("Marshal(Money{}) = %v, want ErrUninitialized", err)
	}
}

func TestMarshalJSON(t *testing.T) {
	tests := []struct {
		m    Money
		want string
	}{
		{mustMoney(t, "25.00", "BRL"), `{"amount":"25.00","currency":"BRL"}`},
		{mustMoney(t, "0.00", "BRL"), `{"amount":"0.00","currency":"BRL"}`},
		{mustMinor(t, -500, BRL), `{"amount":"-5.00","currency":"BRL"}`},
	}
	for _, tt := range tests {
		got, err := json.Marshal(tt.m)
		if err != nil || string(got) != tt.want {
			t.Errorf("Marshal(%s) = %s, %v; want %s", tt.m, got, err, tt.want)
		}
	}
}

func TestUnmarshalJSON(t *testing.T) {
	var m Money
	if err := json.Unmarshal([]byte(`{"amount":"25.00","currency":"BRL"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Minor() != 2500 || m.Currency() != BRL {
		t.Errorf("Unmarshal = %s", m)
	}

	invalid := []struct {
		name string
		body string
		want error
	}{
		{"amount numérico (seria float)", `{"amount":25.00,"currency":"BRL"}`, ErrInvalidAmount},
		{"amount inteiro numérico", `{"amount":25,"currency":"BRL"}`, ErrInvalidAmount},
		{"amount ausente", `{"currency":"BRL"}`, ErrInvalidAmount},
		{"amount null", `{"amount":null,"currency":"BRL"}`, ErrInvalidAmount},
		{"amount vazio", `{"amount":"","currency":"BRL"}`, ErrInvalidAmount},
		{"notação científica", `{"amount":"1e3","currency":"BRL"}`, ErrInvalidAmount},
		{"escala excedente", `{"amount":"1.001","currency":"BRL"}`, ErrInvalidAmount},
		{"negativo", `{"amount":"-1.00","currency":"BRL"}`, ErrNegativeAmount},
		{"moeda ausente", `{"amount":"1.00"}`, ErrInvalidCurrency},
		{"moeda minúscula", `{"amount":"1.00","currency":"brl"}`, ErrInvalidCurrency},
		{"não é objeto", `"25.00"`, ErrInvalidAmount},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			var m Money
			err := json.Unmarshal([]byte(tt.body), &m)
			if !errors.Is(err, tt.want) {
				t.Errorf("Unmarshal(%s) = %v, want %v", tt.body, err, tt.want)
			}
			if m.IsValid() {
				t.Errorf("Money alterado após erro: %s", m)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	for _, amount := range []string{"0.00", "0.01", "25.00", "92233720368547758.07"} {
		orig := mustMoney(t, amount, "BRL")
		data, err := json.Marshal(orig)
		if err != nil {
			t.Fatal(err)
		}
		var back Money
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("round-trip %s: %v", amount, err)
		}
		if !back.Equal(orig) {
			t.Errorf("round-trip %s = %s", orig, back)
		}
	}
}
