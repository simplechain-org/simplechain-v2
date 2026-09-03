//go:build ignore
// +build ignore

// This is a standalone program to verify the reward calculation formulas
// Run with: go run formula_verification.go

package main

import (
	"fmt"
	"math/big"

	"github.com/shopspring/decimal"
)

func CalculateRewardByRate(rate *big.Int, annualBlockCountEveryYear *big.Int,
	annualBlockCountEveryEpoch *big.Int, totalPooled *big.Int) *big.Int {
	rateFloat := decimal.NewFromBigInt(rate, 0)
	rateFloat = rateFloat.Div(decimal.New(10000, 0))
	annualBlocksYear := decimal.NewFromBigInt(annualBlockCountEveryYear, 0)
	annualBlocksEpoch := decimal.NewFromBigInt(annualBlockCountEveryEpoch, 0)
	ratePerBlockFloat := rateFloat.Div(annualBlocksYear)
	fmt.Println("ratePerBlockScale", ratePerBlockFloat.String())
	ratePerBlock := decimal.New(1, 0).Add(ratePerBlockFloat)
	fmt.Println("ratePerBlock", ratePerBlock.String(), "annualBlocksEpoch", annualBlocksEpoch.String())
	compoundRate, err := ratePerBlock.PowWithPrecision(annualBlocksEpoch, 4)
	if err != nil {
		fmt.Println("CalculateRewardByRate", "err", err)
		return big.NewInt(0)
	}
	fmt.Println("compoundRate", compoundRate.Round(6).String())

	//Calculate reward: totalPooled * compoundRate
	rewardFloat := new(big.Float).SetInt(totalPooled)
	rewardFloat.Mul(rewardFloat, big.NewFloat(compoundRate))
	reward, _ := rewardFloat.Int(nil)
	return big.NewInt(100)
}

// CalculateRewardByRateWithBigInt calculates reward using big.Int arithmetic with fast exponentiation
// rate is in basis points (e.g., 500 for 5%)
// Formula: reward = totalPooled * ((1 + rate/10000/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1)
func CalculateRewardByRateWithBigInt(rate *big.Int, annualBlockCountEveryYear *big.Int,
	annualBlockCountEveryEpoch *big.Int, totalPooled *big.Int) *big.Int {

	// Scale factor: 10^13 to maintain precision (4 significant digits for the tiny ratePerBlock)
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(13), nil)

	// ratePerBlock = rate / 10000 / annualBlockCountEveryYear
	// ratePerBlockScaled = rate * scale / 10000 / annualBlockCountEveryYear
	ratePerBlockScaled := new(big.Int).Mul(rate, scale)
	ratePerBlockScaled.Div(ratePerBlockScaled, big.NewInt(10000))
	ratePerBlockScaled.Div(ratePerBlockScaled, annualBlockCountEveryYear)

	// base = (1 + ratePerBlock) * scale = scale + ratePerBlockScaled
	base := new(big.Int).Add(scale, ratePerBlockScaled)

	// Use fast exponentiation to calculate base^annualBlocksEpoch
	// Each multiplication divides by scale to keep the number from growing too large
	result := powerWithScale(base, annualBlockCountEveryEpoch, scale)

	// result is now (1 + ratePerBlock)^annualBlocksEpoch * scale
	//compoundRate = result - scale (this is the actual rate multiplied by scale)
	compoundRate := new(big.Int).Sub(result, scale)

	fmt.Println("compoundRate", result.String())
	reward = totalPooled * compoundRate / scale
	reward := new(big.Int).Mul(totalPooled, compoundRate)
	reward.Div(reward, scale)

	// return reward
	return big.NewInt(100)
}

// powerWithScale calculates base^exp using fast exponentiation (binary exponentiation)
// where base is already scaled up by scale
// Each multiplication is followed by division by scale to maintain precision without overflow
func powerWithScale(base *big.Int, exp *big.Int, scale *big.Int) *big.Int {
	if exp.Cmp(big.NewInt(0)) == 0 {
		return new(big.Int).Set(scale) // base^0 = 1, scaled is scale
	}

	// Initialize result to 1 * scale
	result := new(big.Int).Set(scale)
	currentBase := new(big.Int).Set(base)
	currentExp := new(big.Int).Set(exp)

	// Fast exponentiation algorithm
	for currentExp.Cmp(big.NewInt(0)) > 0 {
		// If current exponent is odd, multiply result by currentBase
		if new(big.Int).And(currentExp, big.NewInt(1)).Cmp(big.NewInt(1)) == 0 {
			// result = (result * currentBase) / scale
			result.Mul(result, currentBase)
			result.Div(result, scale)
		}

		// Square the base: currentBase = (currentBase * currentBase) / scale
		currentBase = new(big.Int).Mul(currentBase, currentBase)
		currentBase.Div(currentBase, scale)

		// Divide exponent by 2 (right shift by 1 bit)
		currentExp.Rsh(currentExp, 1)
	}

	return result
}

func main() {
	fmt.Println("=== Reward Calculation Formula Verification ===\n")

	// Test parameters
	annualBlockCountEveryYear := big.NewInt(42048000)                      // ~365 days worth of blocks
	annualBlockCountEveryEpoch := big.NewInt(115200)                       // ~1 day worth of blocks
	totalPooled := new(big.Int).Mul(big.NewInt(1000000), big.NewInt(1e18)) // 1M tokens

	fmt.Println("Common Parameters:")
	fmt.Printf("  Annual blocks per year: %s\n", annualBlockCountEveryYear.String())
	fmt.Printf("  Annual blocks per epoch: %s\n", annualBlockCountEveryEpoch.String())
	fmt.Printf("  Total pooled: %s wei (1,000,000 tokens)\n\n", totalPooled.String())

	// Test 1: Basic Reward Calculation
	fmt.Println("Test 1: Basic Reward Calculation")
	fmt.Println("Formula: reward = totalPooled * ((1 + rate/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1)")

	nominalInterestRate := big.NewInt(500) // 5%
	//fmt.Printf("  Nominal interest rate: %.2f%%\n", nominalInterestRate*100)

	basicReward := CalculateRewardByRateWithBigInt(nominalInterestRate, annualBlockCountEveryYear, annualBlockCountEveryEpoch, totalPooled)
	// basicRewardFloat := new(big.Float).Quo(new(big.Float).SetInt(basicReward), big.NewFloat(1e18))
	fmt.Printf("  Basic reward: %s wei\n", basicReward.String())
	// fmt.Printf("  Basic reward: %s tokens\n", basicRewardFloat.Text('f', 6))
	// fmt.Println("  ✓ Expected: ~136.89 tokens per day\n")

	// // Test 2: Contribution Reward Rate Calculation
	// fmt.Println("Test 2: Contribution Reward Rate Calculation")
	// fmt.Println("Formula: contributionRewardRate = inflationRate * uptimeRate * (1 - commissionRate) * totalNetworkStakingRatio * sqrt(contributionStakingRatio)")

	// inflationRate := 0.05
	// uptimeRate := 1.0
	// commissionRate := 0.10
	// contributionStakingRatio := 0.80
	// totalNetworkStakingRatio := 0.50

	// fmt.Printf("  Inflation rate: %.2f%%\n", inflationRate*100)
	// fmt.Printf("  Uptime rate: %.2f%%\n", uptimeRate*100)
	// fmt.Printf("  Commission rate: %.2f%%\n", commissionRate*100)
	// fmt.Printf("  Contribution staking ratio: %.2f%%\n", contributionStakingRatio*100)
	// fmt.Printf("  Total network staking ratio: %.2f%%\n", totalNetworkStakingRatio*100)

	// contributionRewardRate := inflationRate * uptimeRate * (1 - commissionRate) *
	// 	totalNetworkStakingRatio * math.Sqrt(contributionStakingRatio)

	// fmt.Printf("  Contribution reward rate: %.6f (%.4f%%)\n", contributionRewardRate, contributionRewardRate*100)

	// contributionReward := CalculateRewardByRate(contributionRewardRate, annualBlockCountEveryYear, annualBlockCountEveryEpoch, totalPooled)
	// contributionRewardFloat := new(big.Float).Quo(new(big.Float).SetInt(contributionReward), big.NewFloat(1e18))
	// fmt.Printf("  Contribution reward: %s wei\n", contributionReward.String())
	// fmt.Printf("  Contribution reward: %s tokens\n", contributionRewardFloat.Text('f', 6))
	// fmt.Println("  ✓ Expected: ~54.96 tokens per day\n")

	// // Test 3: Total Reward
	// totalReward := new(big.Int).Add(basicReward, contributionReward)
	// totalRewardFloat := new(big.Float).Quo(new(big.Float).SetInt(totalReward), big.NewFloat(1e18))
	// fmt.Println("Test 3: Total Reward")
	// fmt.Printf("  Total reward (basic + contribution): %s tokens\n", totalRewardFloat.Text('f', 6))
	// fmt.Println("  ✓ Expected: ~191.85 tokens per day\n")

	// // Test 4: Uptime Rate Calculation
	// fmt.Println("Test 4: Uptime Rate Calculation")
	// fmt.Println("Formula: uptimeRate = inTurnCounts / (inTurnCounts + outTurnCounts)")

	// inTurnCounts := big.NewInt(80)
	// outTurnCounts := big.NewInt(20)
	// totalTurnCounts := new(big.Int).Add(inTurnCounts, outTurnCounts)

	// uptimeRateCalculated := float64(0)
	// if totalTurnCounts.Cmp(big.NewInt(0)) > 0 {
	// 	inTurnFloat := new(big.Float).SetInt(inTurnCounts)
	// 	totalTurnFloat := new(big.Float).SetInt(totalTurnCounts)
	// 	uptimeRateBigFloat := new(big.Float).Quo(inTurnFloat, totalTurnFloat)
	// 	uptimeRateCalculated, _ = uptimeRateBigFloat.Float64()
	// }

	// fmt.Printf("  In-turn counts: %s\n", inTurnCounts.String())
	// fmt.Printf("  Out-turn counts: %s\n", outTurnCounts.String())
	// fmt.Printf("  Uptime rate: %.2f%%\n", uptimeRateCalculated*100)
	// fmt.Println("  ✓ Expected: 80%\n")

	// // Test 5: Staking Ratio Calculations
	// fmt.Println("Test 5: Staking Ratio Calculations")

	// totalDelegated := new(big.Int).Mul(big.NewInt(800000), big.NewInt(1e18))
	// validatorsTotalPooled := new(big.Int).Mul(big.NewInt(50000000), big.NewInt(1e18))
	// totalIssuedSupply := new(big.Int).Mul(big.NewInt(100000000), big.NewInt(1e18))
	// totalBurnedSupply := big.NewInt(0)

	// // Contribution staking ratio: totalDelegated / totalPooled
	// contributionStakingRatioCalc := new(big.Float).SetInt(totalDelegated)
	// contributionStakingRatioCalc.Quo(contributionStakingRatioCalc, new(big.Float).SetInt(totalPooled))
	// contributionRatio, _ := contributionStakingRatioCalc.Float64()

	// // Total network staking ratio: validatorsTotalPooled / (totalIssuedSupply - totalBurnedSupply)
	// totalSupply := new(big.Int).Sub(totalIssuedSupply, totalBurnedSupply)
	// totalNetworkStakingRatioCalc := new(big.Float).SetInt(validatorsTotalPooled)
	// totalNetworkStakingRatioCalc.Quo(totalNetworkStakingRatioCalc, new(big.Float).SetInt(totalSupply))
	// networkRatio, _ := totalNetworkStakingRatioCalc.Float64()

	// fmt.Printf("  Total delegated: %s wei (800,000 tokens)\n", totalDelegated.String())
	// fmt.Printf("  Total pooled: %s wei (1,000,000 tokens)\n", totalPooled.String())
	// fmt.Printf("  Contribution staking ratio: %.2f%%\n", contributionRatio*100)
	// fmt.Println("  ✓ Expected: 80%")

	// fmt.Printf("\n  Validators total pooled: %s wei (50,000,000 tokens)\n", validatorsTotalPooled.String())
	// fmt.Printf("  Total supply: %s wei (100,000,000 tokens)\n", totalSupply.String())
	// fmt.Printf("  Total network staking ratio: %.2f%%\n", networkRatio*100)
	// fmt.Println("  ✓ Expected: 50%\n")

	// // Test 6: Impact of Different Parameters
	// fmt.Println("Test 6: Impact of Different Parameters on Contribution Reward")

	// scenarios := []struct {
	// 	name         string
	// 	uptime       float64
	// 	commission   float64
	// 	contribRatio float64
	// }{
	// 	{"100% uptime, 10% commission", 1.0, 0.10, 0.80},
	// 	{"80% uptime, 10% commission", 0.80, 0.10, 0.80},
	// 	{"100% uptime, 30% commission", 1.0, 0.30, 0.80},
	// 	{"100% uptime, 10% commission, 50% delegated", 1.0, 0.10, 0.50},
	// }

	// for _, scenario := range scenarios {
	// 	rate := inflationRate * scenario.uptime * (1 - scenario.commission) *
	// 		totalNetworkStakingRatio * math.Sqrt(scenario.contribRatio)
	// 	reward := CalculateRewardByRate(rate, annualBlockCountEveryYear, annualBlockCountEveryEpoch, totalPooled)
	// 	rewardFloat := new(big.Float).Quo(new(big.Float).SetInt(reward), big.NewFloat(1e18))

	// 	fmt.Printf("  %s:\n", scenario.name)
	// 	fmt.Printf("    Contribution reward rate: %.6f\n", rate)
	// 	fmt.Printf("    Daily reward: %s tokens\n", rewardFloat.Text('f', 6))
	// }

	fmt.Println("\n=== All Formula Verifications Completed ===")
}
