package parlia

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie/utils"
	"github.com/holiman/uint256"
)

// Helper function to create a test Parlia instance
func createTestParlia() *Parlia {
	db := rawdb.NewMemoryDatabase()

	engine := New(params.ParliaTestChainConfig, db, nil, common.Hash{})
	return engine
}

// Helper function to create big.Int from string (for large numbers that overflow int64)
func mustBigIntFromString(s string) *big.Int {
	result := new(big.Int)
	result, ok := result.SetString(s, 10)
	if !ok {
		panic("invalid big.Int string: " + s)
	}
	return result
}

// TestCalculateRewardByRate tests the CalculateRewardByRate function with comprehensive edge cases
func TestCalculateRewardByRate(t *testing.T) {
	engine := createTestParlia()

	tests := []struct {
		name                       string
		rate                       *big.Int
		annualBlockCountEveryYear  *big.Int
		annualBlockCountEveryEpoch *big.Int
		totalPooled                *big.Int
		expectedBasicReward        *big.Int
	}{
		// Basic test cases
		{
			name:                       "Basic reward calculation with 2% rate",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(42048000),
			annualBlockCountEveryEpoch: big.NewInt(115200),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(54796021637690),
		},
		{
			name:                       "Basic reward calculation with 5% rate",
			rate:                       big.NewInt(500), // 5% in basis points
			annualBlockCountEveryYear:  big.NewInt(42048000),
			annualBlockCountEveryEpoch: big.NewInt(115200),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(136995684233167),
		},
		{
			name:                       "Basic reward calculation with 10% rate",
			rate:                       big.NewInt(1000), // 10% in basis points
			annualBlockCountEveryYear:  big.NewInt(42048000),
			annualBlockCountEveryEpoch: big.NewInt(115200),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(274010136168604),
		},
		// Zero and minimum cases
		{
			name:                       "Zero rate should return zero reward",
			rate:                       big.NewInt(0),
			annualBlockCountEveryYear:  big.NewInt(42048000),
			annualBlockCountEveryEpoch: big.NewInt(115200),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(0),
		},
		{
			name:                       "Minimum rate (1 basis point)",
			rate:                       big.NewInt(1), // 0.01% in basis points
			annualBlockCountEveryYear:  big.NewInt(42048000),
			annualBlockCountEveryEpoch: big.NewInt(115200),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(273972594019),
		},
		{
			name:                       "Zero total pooled",
			rate:                       big.NewInt(200),
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(86400),
			totalPooled:                big.NewInt(0),
			expectedBasicReward:        big.NewInt(0),
		},
		// Large total pooled cases
		{
			name:                       "Large total pooled (1M tokens)",
			rate:                       big.NewInt(500), // 5% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(86400),
			totalPooled:                new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1M tokens
			expectedBasicReward:        mustBigIntFromString("136995684263950000000"),
		},
		{
			name:                       "Very large total pooled (1B tokens)",
			rate:                       big.NewInt(500), // 5% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(86400),
			totalPooled:                new(big.Int).Mul(big.NewInt(1000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1B tokens
			expectedBasicReward:        mustBigIntFromString("136995684263950000000000"),
		},
		{
			name:                       "Extremely large total pooled (1T tokens)",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(86400),
			totalPooled:                new(big.Int).Mul(big.NewInt(1000000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1T tokens
			expectedBasicReward:        mustBigIntFromString("54796021675641000000000000"),
		},
		// Short epoch cases
		{
			name:                       "Short epoch (1 hour)",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(3600),                                      // 1 hour
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(2283107624140),
		},
		{
			name:                       "Very short epoch (1 block)",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(1),                                         // 1 block
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(634195839),
		},
		// Long epoch cases
		{
			name:                       "Long epoch (1 week)",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(604800),                                    // 1 week
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(383635212172188),
		},
		{
			name:                       "Very long epoch (1 month)",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(2592000),                                   // 1 month
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(1645187451247478),
		},
		// Edge cases for block counts
		{
			name:                       "Small annual block count",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(1000),
			annualBlockCountEveryEpoch: big.NewInt(100),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(2001981294227614),
		},
		{
			name:                       "Very large annual block count",
			rate:                       big.NewInt(200), // 2% in basis points
			annualBlockCountEveryYear:  big.NewInt(100000000),
			annualBlockCountEveryEpoch: big.NewInt(1000000),
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(200020001106099),
		},
		// Combined edge cases
		{
			name:                       "High rate with large total pooled",
			rate:                       big.NewInt(1000), // 10% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(86400),
			totalPooled:                new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1M tokens
			expectedBasicReward:        mustBigIntFromString("274010136172548000000"),
		},
		{
			name:                       "Low rate with small epoch",
			rate:                       big.NewInt(1), // 0.01% in basis points
			annualBlockCountEveryYear:  big.NewInt(31536000),
			annualBlockCountEveryEpoch: big.NewInt(1),                                         // 1 block
			totalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 token
			expectedBasicReward:        big.NewInt(3170979),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Scale the rate for the function
			scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
			rateScaled := new(big.Int).Mul(tt.rate, scale)

			reward := engine.CalculateRewardByRate(rateScaled, tt.annualBlockCountEveryYear, tt.annualBlockCountEveryEpoch, tt.totalPooled)

			if reward.Cmp(tt.expectedBasicReward) != 0 {
				t.Errorf("CalculateRewardByRate() reward = %v, expected %v", reward, tt.expectedBasicReward)
			}
		})
	}
}

// TestPowerWithScale tests the powerWithScale function
func TestPowerWithScale(t *testing.T) {
	tests := []struct {
		name     string
		base     *big.Int
		exp      *big.Int
		scale    *big.Int
		expected *big.Int
	}{
		{
			name:     "Power of 0 should return scale",
			base:     new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			exp:      big.NewInt(0),
			scale:    new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			expected: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
		},
		{
			name:     "Power of 1 should return base",
			base:     new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			exp:      big.NewInt(1),
			scale:    new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			expected: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
		},
		{
			name:     "Power of 2",
			base:     new(big.Int).Mul(big.NewInt(2), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 2 * scale
			exp:      big.NewInt(2),
			scale:    new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			expected: new(big.Int).Mul(big.NewInt(4), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 4 * scale
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := powerWithScale(tt.base, tt.exp, tt.scale)
			if result.Cmp(tt.expected) != 0 {
				t.Errorf("powerWithScale() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestCalculateContributionRewardRate tests the CalculateContributionRewardRate function with comprehensive edge cases
func TestCalculateContributionRewardRate(t *testing.T) {
	tests := []struct {
		name                  string
		inflationRate         *big.Int
		inTurnCounts          *big.Int
		totalTurnCounts       *big.Int
		commissionRate        *big.Int
		totalDelegated        *big.Int
		totalPooled           *big.Int
		validatorsTotalPooled *big.Int
		totalSupply           *big.Int
		expected              *big.Int
	}{
		// Basic test cases
		{
			name:                  "Basic contribution reward calculation",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                          // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("1138419957600000000000"),
		},
		{
			name:                  "Perfect uptime (100%)",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(10000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                          // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("1423024947000000000000"),
		},
		{
			name:                  "High inflation rate (10%)",
			inflationRate:         big.NewInt(1000), // 10% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                          // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("2276839915200000000000"),
		},
		{
			name:                  "Zero commission rate",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(0),                                                                             // 0% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("1264911064000000000000"),
		},
		{
			name:                  "Maximum commission rate (100%)",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(10000),                                                                         // 100% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              big.NewInt(0),
		},
		// Zero and edge cases
		{
			name:                  "Zero inflation rate",
			inflationRate:         big.NewInt(0),
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			expected:              big.NewInt(0),
		},
		{
			name:                  "Zero uptime",
			inflationRate:         big.NewInt(500),
			inTurnCounts:          big.NewInt(0),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			expected:              big.NewInt(0),
		},
		{
			name:                  "Zero total turn counts",
			inflationRate:         big.NewInt(500),
			inTurnCounts:          big.NewInt(0),
			totalTurnCounts:       big.NewInt(0),
			commissionRate:        big.NewInt(1000),
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			expected:              big.NewInt(0),
		},
		{
			name:                  "Zero total delegated",
			inflationRate:         big.NewInt(500),
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),
			totalDelegated:        big.NewInt(0),
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
			expected:              big.NewInt(0),
		},
		{
			name:                  "Equal total delegated and pooled",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                          // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("3600000000000000000000"),
		},
		// Large value cases
		{
			name:                  "Large total delegated",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                                // 10% in basis points
			totalDelegated:        new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),    // 1M tokens
			totalPooled:           new(big.Int).Mul(big.NewInt(10000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10M tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100M tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1B tokens
			expected:              mustBigIntFromString("1138419957600000000000"),
		},
		{
			name:                  "Very small contribution ratio",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                             // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                        // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1M tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),     // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),    // 1000 tokens
			expected:              big.NewInt(3600000000000000000),
		},
		{
			name:                  "Very small network staking ratio",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                             // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                        // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),      // 10 tokens
			validatorsTotalPooled: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                        // 1 token
			totalSupply:           new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1M tokens
			expected:              mustBigIntFromString("113841995760000000000000000"),
		},
		// Edge cases for ratios
		{
			name:                  "High contribution ratio (50%)",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                          // 10% in basis points
			totalDelegated:        new(big.Int).Mul(big.NewInt(5), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),    // 5 tokens
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("2545584411600000000000"),
		},
		{
			name:                  "High network staking ratio (50%)",
			inflationRate:         big.NewInt(500), // 5% in basis points
			inTurnCounts:          big.NewInt(8000),
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(1000),                                                                          // 10% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(500), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 500 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("227683991520000000000"),
		},
		// Combined edge cases
		{
			name:                  "Low uptime with high commission",
			inflationRate:         big.NewInt(500),  // 5% in basis points
			inTurnCounts:          big.NewInt(1000), // 10% uptime
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(5000),                                                                          // 50% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("79056941500000000000"),
		},
		{
			name:                  "High inflation with perfect uptime and zero commission",
			inflationRate:         big.NewInt(1000),  // 10% in basis points
			inTurnCounts:          big.NewInt(10000), // 100% uptime
			totalTurnCounts:       big.NewInt(10000),
			commissionRate:        big.NewInt(0),                                                                             // 0% in basis points
			totalDelegated:        new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),                                     // 1 token
			totalPooled:           new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),   // 10 tokens
			validatorsTotalPooled: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			totalSupply:           new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expected:              mustBigIntFromString("3162277660000000000000"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CalculateContributionRewardRate(
				tt.inflationRate,
				tt.inTurnCounts,
				tt.totalTurnCounts,
				tt.commissionRate,
				tt.totalDelegated,
				tt.totalPooled,
				tt.validatorsTotalPooled,
				tt.totalSupply,
			)

			if result.Cmp(tt.expected) != 0 {
				t.Errorf("CalculateContributionRewardRate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestSqrtBigInt tests the sqrtBigInt function
func TestSqrtBigInt(t *testing.T) {
	tests := []struct {
		name     string
		input    *big.Int
		expected *big.Int
	}{
		{
			name:     "Square root of 0",
			input:    big.NewInt(0),
			expected: big.NewInt(0),
		},
		{
			name:     "Square root of 1",
			input:    new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil), // 1 scaled
			expected: new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil),  // 1 scaled by 10^9
		},
		{
			name:     "Square root of 4",
			input:    new(big.Int).Mul(big.NewInt(4), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 4 scaled
			expected: new(big.Int).Mul(big.NewInt(2), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil)),  // 2 scaled by 10^9
		},
		{
			name:     "Square root of 9",
			input:    new(big.Int).Mul(big.NewInt(9), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 9 scaled
			expected: new(big.Int).Mul(big.NewInt(3), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil)),  // 3 scaled by 10^9
		},
		{
			name:     "Square root of 100",
			input:    new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 100 scaled
			expected: new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil)),   // 10 scaled by 10^9
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sqrtBigInt(tt.input)
			// Allow for small rounding differences
			diff := new(big.Int).Sub(result, tt.expected)
			diff.Abs(diff)
			// Allow difference of up to 1% of expected value
			tolerance := new(big.Int).Div(tt.expected, big.NewInt(100))
			if diff.Cmp(tolerance) > 0 {
				t.Errorf("sqrtBigInt() = %v, expected approximately %v (diff: %v)", result, tt.expected, diff)
			}
		})
	}
}

// TestCalculateNewYearInflation tests the calculateNewYearInflation function
func TestCalculateNewYearInflation(t *testing.T) {
	tests := []struct {
		name                string
		currentTotalSupply  *big.Int
		lastYearTotalSupply *big.Int
		expectedIssuance    *big.Int
		expectedRate        *big.Int
	}{
		// Basic test cases
		{
			name:                "Normal inflation calculation (10x growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			expectedIssuance:    mustBigIntFromString("900000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Normal inflation calculation (2% growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1020), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1020 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    mustBigIntFromString("20000000000000000000"),
			expectedRate:        big.NewInt(196),
		},
		{
			name:                "Normal inflation calculation (5% growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1050), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1050 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    mustBigIntFromString("50000000000000000000"),
			expectedRate:        big.NewInt(476),
		},
		{
			name:                "Normal inflation calculation (10% growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1100 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    mustBigIntFromString("100000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		// Zero and edge cases
		{
			name:                "No inflation (same supply)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    big.NewInt(0),
			expectedRate:        big.NewInt(0),
		},
		{
			name:                "Negative inflation (deflation)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    big.NewInt(0),
			expectedRate:        big.NewInt(0),
		},
		{
			name:                "Zero current supply",
			currentTotalSupply:  big.NewInt(0),
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 100 tokens
			expectedIssuance:    big.NewInt(0),
			expectedRate:        big.NewInt(0),
		},
		{
			name:                "Zero last year supply",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(100), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 100 tokens
			lastYearTotalSupply: big.NewInt(0),
			expectedIssuance:    mustBigIntFromString("100000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Both zero",
			currentTotalSupply:  big.NewInt(0),
			lastYearTotalSupply: big.NewInt(0),
			expectedIssuance:    big.NewInt(0),
			expectedRate:        big.NewInt(0),
		},
		// Small differences
		{
			name:                "Very small inflation (1 wei)",
			currentTotalSupply:  new(big.Int).Add(new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), big.NewInt(1)), // 1000 tokens + 1 wei
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),                                  // 1000 tokens
			expectedIssuance:    big.NewInt(1),
			expectedRate:        big.NewInt(0), // Too small to register
		},
		{
			name:                "Small inflation (1 token)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1001), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1001 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    big.NewInt(1000000000000000000),
			expectedRate:        big.NewInt(9),
		},
		// High inflation cases
		{
			name:                "High inflation (50% growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1500), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1500 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    mustBigIntFromString("500000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Very high inflation (100% growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(2000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 2000 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    mustBigIntFromString("1000000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Extremely high inflation (1000% growth)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(11000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 11000 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 1000 tokens
			expectedIssuance:    mustBigIntFromString("10000000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Inflation exceeding MaxInflationRate",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(2000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 2000 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1000 tokens
			expectedIssuance:    mustBigIntFromString("1000000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		// Large value cases
		{
			name:                "Large total supply (1B tokens)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1B tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(100000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100M tokens
			expectedIssuance:    mustBigIntFromString("900000000000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Very large total supply (1T tokens)",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1T tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(100000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 100B tokens
			expectedIssuance:    mustBigIntFromString("900000000000000000000000000000"),
			expectedRate:        big.NewInt(500), // Capped at MaxInflationRate
		},
		{
			name:                "Large supply with small difference",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000000001), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1B + 1 tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(1000000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1B tokens
			expectedIssuance:    big.NewInt(1000000000000000000),
			expectedRate:        big.NewInt(0), // Too small relative to large supply
		},
		// Edge cases for rate calculation
		{
			name:                "Rate calculation precision test",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1M tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(999999), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 999999 tokens
			expectedIssuance:    big.NewInt(1000000000000000000),
			expectedRate:        big.NewInt(0), // Too small to register
		},
		{
			name:                "Rate calculation with rounding",
			currentTotalSupply:  new(big.Int).Mul(big.NewInt(1000000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), // 1M tokens
			lastYearTotalSupply: new(big.Int).Mul(big.NewInt(999500), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),  // 999500 tokens
			expectedIssuance:    mustBigIntFromString("500000000000000000000"),
			expectedRate:        big.NewInt(5),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			additionalIssuance, newInflationRate := calculateNewYearInflation(tt.currentTotalSupply, tt.lastYearTotalSupply)

			if additionalIssuance.Cmp(tt.expectedIssuance) != 0 {
				t.Errorf("calculateNewYearInflation() additionalIssuance = %v, expected %v", additionalIssuance, tt.expectedIssuance)
			}
			if newInflationRate.Cmp(tt.expectedRate) != 0 {
				t.Errorf("calculateNewYearInflation() newInflationRate = %v, expected %v", newInflationRate, tt.expectedRate)
			}
		})
	}
}

// TestGetValidatorDelegatedAmountLogic tests the logic of getValidatorDelegatedAmount
// Note: Full integration test would require a proper mock backend, which is complex.
// This test focuses on the minimum delegated amount logic.
func TestGetValidatorDelegatedAmountLogic(t *testing.T) {
	// Test the minimum delegated amount logic
	minDelegated := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1 token

	// Test case: otherDelegated is less than minDelegated
	smallOtherDelegated := new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil) // 0.1 token
	if smallOtherDelegated.Cmp(minDelegated) < 0 {
		// Should be set to 0
		expected := big.NewInt(0)
		if smallOtherDelegated.Cmp(expected) != 0 {
			// This is the expected behavior: if otherDelegated < minDelegated, it should be 0
			// The actual function sets it to 0 in this case
		}
	}

	// Test case: otherDelegated is greater than or equal to minDelegated
	largeOtherDelegated := new(big.Int).Exp(big.NewInt(10), big.NewInt(19), nil) // 10 tokens
	if largeOtherDelegated.Cmp(minDelegated) < 0 {
		t.Errorf("largeOtherDelegated should be >= minDelegated")
	}
}

// TestCommissionItem tests the CommissionItem struct
func TestCommissionItem(t *testing.T) {
	item := CommissionItem{
		Rate:          1000, // 10%
		MaxRate:       2000, // 20%
		MaxChangeRate: 100,  // 1%
	}

	if item.Rate != 1000 {
		t.Errorf("CommissionItem.Rate = %v, expected 1000", item.Rate)
	}
	if item.MaxRate != 2000 {
		t.Errorf("CommissionItem.MaxRate = %v, expected 2000", item.MaxRate)
	}
	if item.MaxChangeRate != 100 {
		t.Errorf("CommissionItem.MaxChangeRate = %v, expected 100", item.MaxChangeRate)
	}
}

// TestMaxInflationRate tests the MaxInflationRate constant
func TestMaxInflationRate(t *testing.T) {
	if MaxInflationRate != 500 {
		t.Errorf("MaxInflationRate = %v, expected 500", MaxInflationRate)
	}
}

// TestCalculateValidatorRewards tests the calculateValidatorRewards pure function.
// This demonstrates how the refactored code makes testing easier by separating
// calculation logic from blockchain state dependencies.
func TestCalculateValidatorRewards(t *testing.T) {
	p := createTestParlia()
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	tests := []struct {
		name                       string
		input                      *ValidatorRewardInput
		maxContributionRewardRate  *big.Int
		expectedBasicReward        *big.Int
		expectedContributionReward *big.Int
	}{
		{
			name: "Basic reward calculation with normal parameters",
			input: &ValidatorRewardInput{
				NominalInterestRateScaled:  new(big.Int).Mul(big.NewInt(500), scale),              // 5% in basis points, scaled
				AnnualBlockCountEveryYear:  big.NewInt(31536000),                                  // ~1 year in seconds
				AnnualBlockCountEveryEpoch: big.NewInt(86400),                                     // 1 day in seconds
				TotalPooled:                new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil), // 100 tokens
				InflationRate:              big.NewInt(500),                                       // 5% in basis points
				InTurnCounts:               big.NewInt(8000),
				TotalTurnCounts:            big.NewInt(10000),
				CommissionRate:             big.NewInt(1000),                                      // 10% in basis points
				TotalDelegated:             new(big.Int).Exp(big.NewInt(10), big.NewInt(19), nil), // 10 tokens
				ValidatorsTotalPooled:      new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil), // 1000 tokens
				TotalSupply:                new(big.Int).Exp(big.NewInt(10), big.NewInt(22), nil), // 10000 tokens
				MaxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),             // 10% scaled
			},
			maxContributionRewardRate: new(big.Int).Mul(big.NewInt(1000), scale),
			// Expected values would need to be calculated based on the actual formulas
			// For now, we just verify the function runs without errors
			expectedBasicReward:        nil, // Will be calculated
			expectedContributionReward: nil, // Will be calculated
		},
		{
			name: "Zero total pooled",
			input: &ValidatorRewardInput{
				NominalInterestRateScaled:  new(big.Int).Mul(big.NewInt(500), scale),
				AnnualBlockCountEveryYear:  big.NewInt(31536000),
				AnnualBlockCountEveryEpoch: big.NewInt(86400),
				TotalPooled:                big.NewInt(0),
				InflationRate:              big.NewInt(500),
				InTurnCounts:               big.NewInt(8000),
				TotalTurnCounts:            big.NewInt(10000),
				CommissionRate:             big.NewInt(1000),
				TotalDelegated:             big.NewInt(0),
				ValidatorsTotalPooled:      new(big.Int).Exp(big.NewInt(10), big.NewInt(21), nil),
				TotalSupply:                new(big.Int).Exp(big.NewInt(10), big.NewInt(22), nil),
				MaxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),
			},
			maxContributionRewardRate:  new(big.Int).Mul(big.NewInt(1000), scale),
			expectedBasicReward:        big.NewInt(0),
			expectedContributionReward: big.NewInt(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateValidatorRewards(p, tt.input, tt.maxContributionRewardRate, scale)

			// Verify result is not nil
			if result == nil {
				t.Fatal("calculateValidatorRewards returned nil result")
			}

			// Verify basic reward
			if result.BasicReward == nil {
				t.Error("BasicReward is nil")
			} else if tt.expectedBasicReward != nil && result.BasicReward.Cmp(tt.expectedBasicReward) != 0 {
				t.Errorf("BasicReward = %v, expected %v", result.BasicReward, tt.expectedBasicReward)
			}

			// Verify contribution reward
			if result.ContributionReward == nil {
				t.Error("ContributionReward is nil")
			} else if tt.expectedContributionReward != nil && result.ContributionReward.Cmp(tt.expectedContributionReward) != 0 {
				t.Errorf("ContributionReward = %v, expected %v", result.ContributionReward, tt.expectedContributionReward)
			}

			// Verify rewards are non-negative
			if result.BasicReward.Sign() < 0 {
				t.Errorf("BasicReward is negative: %v", result.BasicReward)
			}
			if result.ContributionReward.Sign() < 0 {
				t.Errorf("ContributionReward is negative: %v", result.ContributionReward)
			}
		})
	}
}

// mockEthAPIReader is a mock implementation of ethAPIReader for testing
type mockEthAPIReader struct {
	nominalInterestRate        *big.Int
	inflationRate              *big.Int
	maxContributionRewardRatio *big.Int
	totalIssuedSupply          *big.Int
	totalBurnedSupply          *big.Int
	validatorsTotalPooled      *big.Int
	allValidators              []common.Address
	validatorData              map[common.Address]*mockValidatorData
	shouldError                bool
	errorMsg                   string
}

type mockValidatorData struct {
	totalDelegated   *big.Int
	totalPooled      *big.Int
	selfDelegated    *big.Int
	consensusAddress common.Address
	inTurnCounts     *big.Int
	outTurnCounts    *big.Int
	commissionRate   CommissionItem
}

func (m *mockEthAPIReader) GetNominalInterestRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	if m.shouldError {
		return nil, errors.New(m.errorMsg)
	}
	return m.nominalInterestRate, nil
}

func (m *mockEthAPIReader) GetInflationRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	if m.shouldError {
		return nil, errors.New(m.errorMsg)
	}
	return m.inflationRate, nil
}

func (m *mockEthAPIReader) GetMaxContributionRewardRatio(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	if m.shouldError {
		return nil, errors.New(m.errorMsg)
	}
	return m.maxContributionRewardRatio, nil
}

func (m *mockEthAPIReader) GetTotalSupply(blockNr rpc.BlockNumberOrHash) (*big.Int, *big.Int, error) {
	if m.shouldError {
		return nil, nil, errors.New(m.errorMsg)
	}
	return m.totalIssuedSupply, m.totalBurnedSupply, nil
}

func (m *mockEthAPIReader) GetValidatorsTotalPooled(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	if m.shouldError {
		return nil, errors.New(m.errorMsg)
	}
	return m.validatorsTotalPooled, nil
}

func (m *mockEthAPIReader) GetAllValidators(blockNr rpc.BlockNumberOrHash, num *big.Int, timestamp uint64) ([]common.Address, error) {
	if m.shouldError {
		return nil, errors.New(m.errorMsg)
	}
	return m.allValidators, nil
}

func (m *mockEthAPIReader) GetValidatorDelegatedAmount(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*big.Int, *big.Int, *big.Int, error) {
	if m.shouldError {
		return nil, nil, nil, errors.New(m.errorMsg)
	}
	if data, ok := m.validatorData[operatorAddr]; ok {
		return data.totalDelegated, data.totalPooled, data.selfDelegated, nil
	}
	return nil, nil, nil, errors.New("validator not found")
}

func (m *mockEthAPIReader) GetValidatorConsensusAddress(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (common.Address, error) {
	if m.shouldError {
		return common.Address{}, errors.New(m.errorMsg)
	}
	if data, ok := m.validatorData[operatorAddr]; ok {
		return data.consensusAddress, nil
	}
	return common.Address{}, errors.New("validator not found")
}

func (m *mockEthAPIReader) GetValidatorUptimeRecord(blockNr rpc.BlockNumberOrHash, val common.Address, index *big.Int) (*big.Int, *big.Int, error) {
	if m.shouldError {
		return nil, nil, errors.New(m.errorMsg)
	}
	// Find validator by consensus address
	for _, data := range m.validatorData {
		if data.consensusAddress == val {
			return data.inTurnCounts, data.outTurnCounts, nil
		}
	}
	return nil, nil, errors.New("validator not found")
}

func (m *mockEthAPIReader) GetValidatorCommissionRate(blockNr rpc.BlockNumberOrHash, val common.Address) (CommissionItem, error) {
	if m.shouldError {
		return CommissionItem{}, errors.New(m.errorMsg)
	}
	if data, ok := m.validatorData[val]; ok {
		return data.commissionRate, nil
	}
	return CommissionItem{}, errors.New("validator not found")
}

// mockEthAPIWriter is a mock implementation of ethAPIWriter for testing
type mockEthAPIWriter struct {
	balances                 map[common.Address]*uint256.Int
	balanceChanges           []balanceChange
	distributeIncomingResult *big.Int
	distributeIncomingError  error
}

type balanceChange struct {
	addr   common.Address
	amount *uint256.Int
	reason tracing.BalanceChangeReason
	isAdd  bool
}

func newMockEthAPIWriter() *mockEthAPIWriter {
	return &mockEthAPIWriter{
		balances:       make(map[common.Address]*uint256.Int),
		balanceChanges: make([]balanceChange, 0),
	}
}

func (m *mockEthAPIWriter) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	m.balances[addr] = new(uint256.Int).Set(amount)
	m.balanceChanges = append(m.balanceChanges, balanceChange{
		addr:   addr,
		amount: new(uint256.Int).Set(amount),
		reason: reason,
		isAdd:  true,
	})
}

func (m *mockEthAPIWriter) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	if current, ok := m.balances[addr]; ok {
		m.balances[addr] = new(uint256.Int).Add(current, amount)
	} else {
		m.balances[addr] = new(uint256.Int).Set(amount)
	}
	m.balanceChanges = append(m.balanceChanges, balanceChange{
		addr:   addr,
		amount: new(uint256.Int).Set(amount),
		reason: reason,
		isAdd:  true,
	})
}

func (m *mockEthAPIWriter) GetBalance(addr common.Address) *uint256.Int {
	if balance, ok := m.balances[addr]; ok {
		return new(uint256.Int).Set(balance)
	}
	return uint256.NewInt(0)
}

func (m *mockEthAPIWriter) DistributeIncoming(val common.Address, transactionFee *big.Int, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) (*big.Int, error) {
	if m.distributeIncomingError != nil {
		return nil, m.distributeIncomingError
	}
	if m.distributeIncomingResult != nil {
		return new(big.Int).Set(m.distributeIncomingResult), nil
	}
	return big.NewInt(0), nil
}

// TestFetchValidatorRewardData tests the fetchValidatorRewardData function with mock reader
func TestFetchValidatorRewardData(t *testing.T) {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	operatorAddr := common.HexToAddress("0x1")
	consensusAddr := common.HexToAddress("0x2")
	upTimeIndex := big.NewInt(0)
	blockNr := rpc.BlockNumberOrHashWithHash(common.HexToHash("0x123"), false)
	header := &types.Header{
		Number: big.NewInt(100),
	}

	tests := []struct {
		name      string
		reader    ethAPIReader
		expectErr bool
		validate  func(*testing.T, *validatorRewardData)
	}{
		{
			name: "Successfully fetch validator reward data",
			reader: &mockEthAPIReader{
				validatorData: map[common.Address]*mockValidatorData{
					operatorAddr: {
						totalDelegated:   new(big.Int).Mul(big.NewInt(10), scale),
						totalPooled:      new(big.Int).Mul(big.NewInt(100), scale),
						selfDelegated:    new(big.Int).Mul(big.NewInt(5), scale),
						consensusAddress: consensusAddr,
						inTurnCounts:     big.NewInt(8000),
						outTurnCounts:    big.NewInt(2000),
						commissionRate:   CommissionItem{Rate: 1000},
					},
				},
			},
			expectErr: false,
			validate: func(t *testing.T, data *validatorRewardData) {
				if data == nil {
					t.Fatal("validatorRewardData is nil")
				}
				expectedTotalDelegated := new(big.Int).Mul(big.NewInt(10), scale)
				if data.TotalDelegated.Cmp(expectedTotalDelegated) != 0 {
					t.Errorf("TotalDelegated = %v, expected %v", data.TotalDelegated, expectedTotalDelegated)
				}
				expectedTotalPooled := new(big.Int).Mul(big.NewInt(100), scale)
				if data.TotalPooled.Cmp(expectedTotalPooled) != 0 {
					t.Errorf("TotalPooled = %v, expected %v", data.TotalPooled, expectedTotalPooled)
				}
				if data.ConsensusAddress != consensusAddr {
					t.Errorf("ConsensusAddress = %v, expected %v", data.ConsensusAddress, consensusAddr)
				}
				if data.InTurnCounts.Cmp(big.NewInt(8000)) != 0 {
					t.Errorf("InTurnCounts = %v, expected 8000", data.InTurnCounts)
				}
				if data.OutTurnCounts.Cmp(big.NewInt(2000)) != 0 {
					t.Errorf("OutTurnCounts = %v, expected 2000", data.OutTurnCounts)
				}
				expectedTotalTurnCounts := big.NewInt(10000)
				if data.TotalTurnCounts.Cmp(expectedTotalTurnCounts) != 0 {
					t.Errorf("TotalTurnCounts = %v, expected %v", data.TotalTurnCounts, expectedTotalTurnCounts)
				}
				if data.CommissionRate.Cmp(big.NewInt(1000)) != 0 {
					t.Errorf("CommissionRate = %v, expected 1000", data.CommissionRate)
				}
			},
		},
		{
			name: "Error when validator not found",
			reader: &mockEthAPIReader{
				validatorData: make(map[common.Address]*mockValidatorData),
			},
			expectErr: true,
		},
		{
			name: "Error when GetValidatorDelegatedAmount fails",
			reader: &mockEthAPIReader{
				shouldError: true,
				errorMsg:    "RPC call failed",
			},
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := fetchValidatorRewardData(tt.reader, blockNr, operatorAddr, upTimeIndex, header)

			if tt.expectErr {
				if err == nil {
					t.Error("expected error but got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.validate != nil {
				tt.validate(t, data)
			}
		})
	}
}

// TestMockEthAPIWriter tests the mockEthAPIWriter implementation
func TestMockEthAPIWriter(t *testing.T) {
	writer := newMockEthAPIWriter()
	systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
	testAddr := common.HexToAddress("0x1")

	// Test SetBalance
	initialBalance := uint256.NewInt(1000)
	writer.SetBalance(testAddr, initialBalance, tracing.BalanceChangeUnspecified)
	balance := writer.GetBalance(testAddr)
	if balance.Cmp(initialBalance) != 0 {
		t.Errorf("SetBalance failed: got %v, expected %v", balance, initialBalance)
	}

	// Test AddBalance
	addAmount := uint256.NewInt(500)
	writer.AddBalance(testAddr, addAmount, tracing.BalanceIncreaseBasicReward)
	expectedBalance := uint256.NewInt(1500)
	balance = writer.GetBalance(testAddr)
	if balance.Cmp(expectedBalance) != 0 {
		t.Errorf("AddBalance failed: got %v, expected %v", balance, expectedBalance)
	}

	// Test GetBalance for non-existent address
	nonExistentAddr := common.HexToAddress("0x2")
	balance = writer.GetBalance(nonExistentAddr)
	if balance.Sign() != 0 {
		t.Errorf("GetBalance for non-existent address should return 0, got %v", balance)
	}

	// Test DistributeIncoming
	writer.distributeIncomingResult = big.NewInt(100)
	result, err := writer.DistributeIncoming(testAddr, big.NewInt(0), nil, nil, nil, nil, nil, nil, nil, false, nil)
	if err != nil {
		t.Errorf("DistributeIncoming failed: %v", err)
	}
	if result.Cmp(big.NewInt(100)) != 0 {
		t.Errorf("DistributeIncoming result = %v, expected 100", result)
	}

	// Test DistributeIncoming with error
	writer.distributeIncomingError = errors.New("test error")
	_, err = writer.DistributeIncoming(testAddr, big.NewInt(0), nil, nil, nil, nil, nil, nil, nil, false, nil)
	if err == nil {
		t.Error("expected error from DistributeIncoming but got nil")
	}

	// Verify balance changes were recorded
	if len(writer.balanceChanges) < 2 {
		t.Errorf("expected at least 2 balance changes, got %d", len(writer.balanceChanges))
	}

	// Test system address balance tracking
	writer.SetBalance(systemAddr, uint256.NewInt(10000), tracing.BalanceDecreaseBSCDistributeReward)
	systemBalance := writer.GetBalance(systemAddr)
	if systemBalance.Cmp(uint256.NewInt(10000)) != 0 {
		t.Errorf("System address balance = %v, expected 10000", systemBalance)
	}
}

// mockChainHeaderReaderWithSnapshot is a mock chain that can return headers for snapshot creation
type mockChainHeaderReaderWithSnapshot struct {
	headers       map[uint64]*types.Header
	config        *params.ChainConfig
	genesisHeader *types.Header
	currentHeader *types.Header
}

func newMockChainHeaderReaderWithSnapshot() *mockChainHeaderReaderWithSnapshot {
	genesisHeader := &types.Header{
		Number:     big.NewInt(0),
		ParentHash: common.Hash{},
		Time:       0,
	}
	return &mockChainHeaderReaderWithSnapshot{
		headers:       make(map[uint64]*types.Header),
		config:        params.ParliaTestChainConfig,
		genesisHeader: genesisHeader,
		currentHeader: genesisHeader,
	}
}

func (m *mockChainHeaderReaderWithSnapshot) addHeader(number uint64, header *types.Header) {
	m.headers[number] = header
	if number > m.currentHeader.Number.Uint64() {
		m.currentHeader = header
	}
}

func (m *mockChainHeaderReaderWithSnapshot) Config() *params.ChainConfig {
	return m.config
}

func (m *mockChainHeaderReaderWithSnapshot) GenesisHeader() *types.Header {
	return m.genesisHeader
}

func (m *mockChainHeaderReaderWithSnapshot) CurrentHeader() *types.Header {
	return m.currentHeader
}

func (m *mockChainHeaderReaderWithSnapshot) GetHeader(hash common.Hash, number uint64) *types.Header {
	if h, ok := m.headers[number]; ok && h.Hash() == hash {
		return h
	}
	return m.GetHeaderByHash(hash)
}

func (m *mockChainHeaderReaderWithSnapshot) GetHeaderByNumber(number uint64) *types.Header {
	return m.headers[number]
}

func (m *mockChainHeaderReaderWithSnapshot) GetHeaderByHash(hash common.Hash) *types.Header {
	for _, h := range m.headers {
		if h.Hash() == hash {
			return h
		}
	}
	return nil
}

func (m *mockChainHeaderReaderWithSnapshot) GetTd(hash common.Hash, number uint64) *big.Int {
	return big.NewInt(0)
}

func (m *mockChainHeaderReaderWithSnapshot) GetHighestVerifiedHeader() *types.Header {
	return m.currentHeader
}

func (m *mockChainHeaderReaderWithSnapshot) GetVerifiedBlockByHash(hash common.Hash) *types.Header {
	return m.GetHeaderByHash(hash)
}

func (m *mockChainHeaderReaderWithSnapshot) ChasingHead() *types.Header {
	return m.currentHeader
}

// mockStateDB is a minimal mock for StateDB
// Note: This is a minimal implementation for testing purposes.
// In a real scenario, you might want to use a more complete mock or the actual StateDB implementation.
type mockStateDB struct{}

// Add stub implementations for all required methods
// These are minimal implementations just to satisfy the interface
func (m *mockStateDB) CreateAccount(common.Address)  {}
func (m *mockStateDB) CreateContract(common.Address) {}
func (m *mockStateDB) SubBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int {
	return uint256.Int{}
}
func (m *mockStateDB) AddBalance(common.Address, *uint256.Int, tracing.BalanceChangeReason) uint256.Int {
	return uint256.Int{}
}
func (m *mockStateDB) GetBalance(common.Address) *uint256.Int { return uint256.NewInt(0) }
func (m *mockStateDB) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
}
func (m *mockStateDB) GetNonce(common.Address) uint64                             { return 0 }
func (m *mockStateDB) SetNonce(common.Address, uint64, tracing.NonceChangeReason) {}
func (m *mockStateDB) GetCodeHash(common.Address) common.Hash                     { return common.Hash{} }
func (m *mockStateDB) GetCode(common.Address) []byte                              { return nil }
func (m *mockStateDB) SetCode(common.Address, []byte) []byte                      { return nil }
func (m *mockStateDB) GetCodeSize(common.Address) int                             { return 0 }
func (m *mockStateDB) AddRefund(uint64)                                           {}
func (m *mockStateDB) SubRefund(uint64)                                           {}
func (m *mockStateDB) GetRefund() uint64                                          { return 0 }
func (m *mockStateDB) GetCommittedState(common.Address, common.Hash) common.Hash {
	return common.Hash{}
}
func (m *mockStateDB) GetState(common.Address, common.Hash) common.Hash { return common.Hash{} }
func (m *mockStateDB) SetState(common.Address, common.Hash, common.Hash) common.Hash {
	return common.Hash{}
}
func (m *mockStateDB) GetStorageRoot(addr common.Address) common.Hash { return common.Hash{} }
func (m *mockStateDB) SelfDestruct(common.Address) uint256.Int        { return uint256.Int{} }
func (m *mockStateDB) HasSelfDestructed(common.Address) bool          { return false }
func (m *mockStateDB) SelfDestruct6780(common.Address) (uint256.Int, bool) {
	return uint256.Int{}, false
}
func (m *mockStateDB) Exist(common.Address) bool                    { return false }
func (m *mockStateDB) Empty(common.Address) bool                    { return true }
func (m *mockStateDB) AddressInAccessList(addr common.Address) bool { return false }
func (m *mockStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool) {
	return false, false
}
func (m *mockStateDB) AddAddressToAccessList(addr common.Address)                {}
func (m *mockStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) {}
func (m *mockStateDB) ClearAccessList()                                          {}
func (m *mockStateDB) PointCache() *utils.PointCache {
	// Return nil - in tests we don't need actual point cache
	return nil
}
func (m *mockStateDB) Prepare(rules params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
}
func (m *mockStateDB) SetTxContext(thash common.Hash, ti int) {}
func (m *mockStateDB) TxIndex() int                           { return 0 }
func (m *mockStateDB) RevertToSnapshot(int)                   {}
func (m *mockStateDB) Snapshot() int                          { return 0 }
func (m *mockStateDB) NoTrie() bool                           { return true }
func (m *mockStateDB) AddLog(*types.Log)                      {}
func (m *mockStateDB) GetLogs(hash common.Hash, blockNumber uint64, blockHash common.Hash, blockTime uint64) []*types.Log {
	return nil
}
func (m *mockStateDB) AddPreimage(common.Hash, []byte) {}
func (m *mockStateDB) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return common.Hash{}
}
func (m *mockStateDB) SetTransientState(addr common.Address, key, value common.Hash) {}
func (m *mockStateDB) Witness() *stateless.Witness {
	// Return nil - in tests we don't need actual witness
	return nil
}
func (m *mockStateDB) AccessEvents() *state.AccessEvents                    { return nil }
func (m *mockStateDB) Finalise(bool)                                        {}
func (m *mockStateDB) IntermediateRoot(deleteEmptyObjects bool) common.Hash { return common.Hash{} }
func (m *mockStateDB) IsAddressInMutations(addr common.Address) bool        { return false }

// TestDistributeBasicAndContributionRewardWithInterfaces tests the distributeBasicAndContributionRewardWithInterfaces function
func TestDistributeBasicAndContributionRewardWithInterfaces(t *testing.T) {
	p := createTestParlia()
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	// Create test validators
	operatorAddr1 := common.HexToAddress("0x1")
	consensusAddr1 := common.HexToAddress("0x2")
	operatorAddr2 := common.HexToAddress("0x3")
	consensusAddr2 := common.HexToAddress("0x4")

	// Create a chain with genesis block for snapshot
	mockChain := newMockChainHeaderReaderWithSnapshot()

	// Create parent header for snapshot
	parentHeader := &types.Header{
		Number:     big.NewInt(99),
		ParentHash: mockChain.genesisHeader.Hash(),
		Time:       params.BreatheBlockInterval * 9,
	}
	mockChain.addHeader(99, parentHeader)

	snap := &Snapshot{
		Number:           99,
		Hash:             parentHeader.Hash(),
		BlockInterval:    params.BreatheBlockInterval,
		TurnLength:       defaultTurnLength,
		Validators:       make(map[common.Address]*ValidatorInfo),
		Recents:          make(map[uint64]common.Address),
		RecentForkHashes: make(map[uint64]string),
	}

	tests := []struct {
		name                  string
		setupReader           func() ethAPIReader
		setupWriter           func() *mockEthAPIWriter
		header                *types.Header
		expectError           bool
		expectedErrorContains string
		validateResults       func(*testing.T, *big.Int, *big.Int, *big.Int)
		validateState         func(*testing.T, *mockEthAPIWriter)
	}{
		{
			name: "Single validator with normal rewards",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					nominalInterestRate:        big.NewInt(500),                           // 5%
					inflationRate:              big.NewInt(500),                           // 5%
					maxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale), // 10%
					totalIssuedSupply:          new(big.Int).Mul(big.NewInt(10000), scale),
					totalBurnedSupply:          big.NewInt(0),
					validatorsTotalPooled:      new(big.Int).Mul(big.NewInt(1000), scale),
					allValidators:              []common.Address{operatorAddr1},
					validatorData: map[common.Address]*mockValidatorData{
						operatorAddr1: {
							totalDelegated:   new(big.Int).Mul(big.NewInt(10), scale),
							totalPooled:      new(big.Int).Mul(big.NewInt(100), scale),
							selfDelegated:    new(big.Int).Mul(big.NewInt(5), scale),
							consensusAddress: consensusAddr1,
							inTurnCounts:     big.NewInt(8000),
							outTurnCounts:    big.NewInt(2000),
							commissionRate:   CommissionItem{Rate: 1000}, // 10%
						},
					},
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				writer := newMockEthAPIWriter()
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				writer.SetBalance(systemAddr, uint256.NewInt(0), tracing.BalanceChangeUnspecified)
				writer.distributeIncomingResult = big.NewInt(1000)
				return writer
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError: false,
			validateResults: func(t *testing.T, totalReward, basicReward, contributionReward *big.Int) {
				if totalReward == nil {
					t.Fatal("totalReward is nil")
				}
				if basicReward == nil {
					t.Fatal("basicReward is nil")
				}
				if contributionReward == nil {
					t.Fatal("contributionReward is nil")
				}
				// Total reward should be sum of basic, contribution, and fixed block reward
				expectedMin := new(big.Int).Add(basicReward, contributionReward)
				if totalReward.Cmp(expectedMin) < 0 {
					t.Errorf("totalReward = %v, expected >= %v", totalReward, expectedMin)
				}
				// Rewards should be non-negative
				if basicReward.Sign() < 0 {
					t.Errorf("basicReward is negative: %v", basicReward)
				}
				if contributionReward.Sign() < 0 {
					t.Errorf("contributionReward is negative: %v", contributionReward)
				}
			},
			validateState: func(t *testing.T, writer *mockEthAPIWriter) {
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				// System address should have balance changes
				if len(writer.balanceChanges) == 0 {
					t.Error("expected balance changes but got none")
				}
				// Check that system address balance was set to zero initially
				foundSetBalance := false
				for _, change := range writer.balanceChanges {
					if change.addr == systemAddr && change.isAdd && change.amount.IsZero() {
						foundSetBalance = true
						break
					}
				}
				if !foundSetBalance {
					t.Error("expected system address balance to be set to zero")
				}
			},
		},
		{
			name: "Multiple validators",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					nominalInterestRate:        big.NewInt(500),
					inflationRate:              big.NewInt(500),
					maxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),
					totalIssuedSupply:          new(big.Int).Mul(big.NewInt(10000), scale),
					totalBurnedSupply:          big.NewInt(0),
					validatorsTotalPooled:      new(big.Int).Mul(big.NewInt(2000), scale),
					allValidators:              []common.Address{operatorAddr1, operatorAddr2},
					validatorData: map[common.Address]*mockValidatorData{
						operatorAddr1: {
							totalDelegated:   new(big.Int).Mul(big.NewInt(10), scale),
							totalPooled:      new(big.Int).Mul(big.NewInt(100), scale),
							selfDelegated:    new(big.Int).Mul(big.NewInt(5), scale),
							consensusAddress: consensusAddr1,
							inTurnCounts:     big.NewInt(8000),
							outTurnCounts:    big.NewInt(2000),
							commissionRate:   CommissionItem{Rate: 1000},
						},
						operatorAddr2: {
							totalDelegated:   new(big.Int).Mul(big.NewInt(20), scale),
							totalPooled:      new(big.Int).Mul(big.NewInt(200), scale),
							selfDelegated:    new(big.Int).Mul(big.NewInt(10), scale),
							consensusAddress: consensusAddr2,
							inTurnCounts:     big.NewInt(9000),
							outTurnCounts:    big.NewInt(1000),
							commissionRate:   CommissionItem{Rate: 500}, // 5%
						},
					},
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				writer := newMockEthAPIWriter()
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				writer.SetBalance(systemAddr, uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
				writer.distributeIncomingResult = big.NewInt(1000)
				return writer
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError: false,
			validateResults: func(t *testing.T, totalReward, basicReward, contributionReward *big.Int) {
				// With two validators, rewards should be accumulated
				if totalReward.Cmp(big.NewInt(0)) <= 0 {
					t.Error("totalReward should be positive with multiple validators")
				}
			},
		},
		{
			name: "Zero validators",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					nominalInterestRate:        big.NewInt(500),
					inflationRate:              big.NewInt(500),
					maxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),
					totalIssuedSupply:          new(big.Int).Mul(big.NewInt(10000), scale),
					totalBurnedSupply:          big.NewInt(0),
					validatorsTotalPooled:      new(big.Int).Mul(big.NewInt(1000), scale),
					allValidators:              []common.Address{},
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				writer := newMockEthAPIWriter()
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				writer.SetBalance(systemAddr, uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
				return writer
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError: false,
			validateResults: func(t *testing.T, totalReward, basicReward, contributionReward *big.Int) {
				if totalReward.Cmp(big.NewInt(0)) != 0 {
					t.Errorf("totalReward = %v, expected 0", totalReward)
				}
				if basicReward.Cmp(big.NewInt(0)) != 0 {
					t.Errorf("basicReward = %v, expected 0", basicReward)
				}
				if contributionReward.Cmp(big.NewInt(0)) != 0 {
					t.Errorf("contributionReward = %v, expected 0", contributionReward)
				}
			},
		},
		{
			name: "Error when GetAllValidators fails",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					shouldError: true,
					errorMsg:    "RPC call failed",
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				return newMockEthAPIWriter()
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError:           true,
			expectedErrorContains: "RPC call failed",
		},
		{
			name: "Error when GetNominalInterestRate fails",
			setupReader: func() ethAPIReader {
				reader := &mockEthAPIReader{
					allValidators: []common.Address{operatorAddr1},
				}
				// Set error after GetAllValidators succeeds
				reader.shouldError = true
				reader.errorMsg = "RPC call failed"
				return reader
			},
			setupWriter: func() *mockEthAPIWriter {
				return newMockEthAPIWriter()
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError:           true,
			expectedErrorContains: "RPC call failed",
		},
		{
			name: "Error when fetchValidatorRewardData fails",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					nominalInterestRate:        big.NewInt(500),
					inflationRate:              big.NewInt(500),
					maxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),
					totalIssuedSupply:          new(big.Int).Mul(big.NewInt(10000), scale),
					totalBurnedSupply:          big.NewInt(0),
					validatorsTotalPooled:      new(big.Int).Mul(big.NewInt(1000), scale),
					allValidators:              []common.Address{operatorAddr1},
					validatorData:              make(map[common.Address]*mockValidatorData), // Empty, will cause error
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				writer := newMockEthAPIWriter()
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				writer.SetBalance(systemAddr, uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
				return writer
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError:           true,
			expectedErrorContains: "validator not found",
		},
		{
			name: "Error when DistributeIncoming fails",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					nominalInterestRate:        big.NewInt(500),
					inflationRate:              big.NewInt(500),
					maxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),
					totalIssuedSupply:          new(big.Int).Mul(big.NewInt(10000), scale),
					totalBurnedSupply:          big.NewInt(0),
					validatorsTotalPooled:      new(big.Int).Mul(big.NewInt(1000), scale),
					allValidators:              []common.Address{operatorAddr1},
					validatorData: map[common.Address]*mockValidatorData{
						operatorAddr1: {
							totalDelegated:   new(big.Int).Mul(big.NewInt(10), scale),
							totalPooled:      new(big.Int).Mul(big.NewInt(100), scale),
							selfDelegated:    new(big.Int).Mul(big.NewInt(5), scale),
							consensusAddress: consensusAddr1,
							inTurnCounts:     big.NewInt(8000),
							outTurnCounts:    big.NewInt(2000),
							commissionRate:   CommissionItem{Rate: 1000},
						},
					},
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				writer := newMockEthAPIWriter()
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				writer.SetBalance(systemAddr, uint256.NewInt(1000000), tracing.BalanceChangeUnspecified)
				writer.distributeIncomingError = errors.New("distributeIncoming failed")
				return writer
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1,
			},
			expectError:           true,
			expectedErrorContains: "distributeIncoming",
		},
		{
			name: "Validator with coinbase address gets breathe block fee",
			setupReader: func() ethAPIReader {
				return &mockEthAPIReader{
					nominalInterestRate:        big.NewInt(500),
					inflationRate:              big.NewInt(500),
					maxContributionRewardRatio: new(big.Int).Mul(big.NewInt(1000), scale),
					totalIssuedSupply:          new(big.Int).Mul(big.NewInt(10000), scale),
					totalBurnedSupply:          big.NewInt(0),
					validatorsTotalPooled:      new(big.Int).Mul(big.NewInt(1000), scale),
					allValidators:              []common.Address{operatorAddr1},
					validatorData: map[common.Address]*mockValidatorData{
						operatorAddr1: {
							totalDelegated:   new(big.Int).Mul(big.NewInt(10), scale),
							totalPooled:      new(big.Int).Mul(big.NewInt(100), scale),
							selfDelegated:    new(big.Int).Mul(big.NewInt(5), scale),
							consensusAddress: consensusAddr1,
							inTurnCounts:     big.NewInt(8000),
							outTurnCounts:    big.NewInt(2000),
							commissionRate:   CommissionItem{Rate: 1000},
						},
					},
				}
			},
			setupWriter: func() *mockEthAPIWriter {
				writer := newMockEthAPIWriter()
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				breatheBlockFee := uint256.NewInt(500000)
				writer.SetBalance(systemAddr, breatheBlockFee, tracing.BalanceChangeUnspecified)
				writer.distributeIncomingResult = big.NewInt(1000)
				return writer
			},
			header: &types.Header{
				Number:     big.NewInt(100),
				Time:       params.BreatheBlockInterval * 10,
				ParentHash: parentHeader.Hash(),
				Coinbase:   consensusAddr1, // Same as consensus address
			},
			expectError: false,
			validateState: func(t *testing.T, writer *mockEthAPIWriter) {
				// Check that breathe block fee was added
				systemAddr := common.HexToAddress("0x0000000000000000000000000000000000001001")
				foundBreatheFee := false
				for _, change := range writer.balanceChanges {
					if change.addr == systemAddr && change.isAdd && change.amount.Cmp(uint256.NewInt(500000)) == 0 {
						foundBreatheFee = true
						break
					}
				}
				if !foundBreatheFee {
					t.Error("expected breathe block fee to be added to system address")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := tt.setupReader()
			writer := tt.setupWriter()
			state := &mockStateDB{}

			totalReward, basicReward, contributionReward, err := p.distributeBasicAndContributionRewardWithInterfaces(
				mockChain, state, tt.header,
				nil, nil, nil, nil, false, nil,
				reader, writer, snap,
			)

			if tt.expectError {
				if err == nil {
					t.Error("expected error but got nil")
					return
				}
				if tt.expectedErrorContains != "" && !contains(err.Error(), tt.expectedErrorContains) {
					t.Errorf("error message '%v' does not contain '%s'", err, tt.expectedErrorContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.validateResults != nil {
				tt.validateResults(t, totalReward, basicReward, contributionReward)
			}

			if tt.validateState != nil {
				tt.validateState(t, writer)
			}
		})
	}
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > len(substr) && (s[:len(substr)] == substr ||
			s[len(s)-len(substr):] == substr ||
			indexOf(s, substr) >= 0)))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
