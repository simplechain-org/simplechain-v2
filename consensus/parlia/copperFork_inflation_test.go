package parlia

import (
	"math/big"
	"testing"
)

func TestCalculateNewYearInflation_Basic(t *testing.T) {
	// lastYear = 1000e18, current = 1050e18 -> delta = 50e18, rate = 50/1000*10000 = 500
	lastYear := new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18))
	current := new(big.Int).Mul(big.NewInt(1050), big.NewInt(1e18))

	delta, rate := calculateNewYearInflation(current, lastYear)

	wantDelta := new(big.Int).Mul(big.NewInt(50), big.NewInt(1e18))
	if delta.Cmp(wantDelta) != 0 {
		t.Fatalf("unexpected delta, want %s, got %s", wantDelta.String(), delta.String())
	}
	if rate.Cmp(big.NewInt(500)) != 0 {
		t.Fatalf("unexpected rate, want 500, got %s", rate.String())
	}
}

func TestCalculateNewYearInflation_NoIncrease(t *testing.T) {
	// lastYear = current -> delta = 0, rate = 0
	amount := new(big.Int).Mul(big.NewInt(1234567), big.NewInt(1e18))
	delta, rate := calculateNewYearInflation(amount, amount)

	if delta.Sign() != 0 {
		t.Fatalf("unexpected delta, want 0, got %s", delta.String())
	}
	if rate.Sign() != 0 {
		t.Fatalf("unexpected rate, want 0, got %s", rate.String())
	}
}

func TestCalculateNewYearInflation_LargeNumbers(t *testing.T) {
	// Use large 18-decimal amounts to ensure no overflow and correct scaling
	// lastYear = 10^26, current = lastYear + 10^24 => rate = (1e24 / 1e26) * 10000 = 100
	lastYear := new(big.Int)
	lastYear.Exp(big.NewInt(10), big.NewInt(26), nil)
	inc := new(big.Int)
	inc.Exp(big.NewInt(10), big.NewInt(24), nil)
	current := new(big.Int).Add(lastYear, inc)

	delta, rate := calculateNewYearInflation(current, lastYear)

	if delta.Cmp(inc) != 0 {
		t.Fatalf("unexpected delta, want %s, got %s", inc.String(), delta.String())
	}
	if rate.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("unexpected rate, want 100, got %s", rate.String())
	}
}
