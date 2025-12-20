package parlia

import (
	"math/big"
	"testing"
)

// BenchmarkCalculateRewardByRate_Scale benchmarks CalculateRewardByRate with scale data
func BenchmarkCalculateRewardByRate_Scale(b *testing.B) {
	engine := createTestParlia()
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	rate := big.NewInt(500) // 5% in basis points
	rateScaled := new(big.Int).Mul(rate, scale)
	annualBlockCountEveryYear := big.NewInt(31536000)                    // ~1 year
	annualBlockCountEveryEpoch := big.NewInt(86400)                      // 1 day
	totalPooled := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1 token

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = engine.CalculateRewardByRate(rateScaled, annualBlockCountEveryYear, annualBlockCountEveryEpoch, totalPooled)
	}
}
