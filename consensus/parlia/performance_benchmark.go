//go:build ignore

// Standalone performance benchmark for powerWithScale
// Run with: go run performance_benchmark.go

package main

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

// powerWithScale calculates base^exp using fast exponentiation (binary exponentiation)
func powerWithScale(base *big.Int, exp *big.Int, scale *big.Int) *big.Int {
	if exp.Cmp(big.NewInt(0)) == 0 {
		return new(big.Int).Set(scale)
	}

	result := new(big.Int).Set(scale)
	currentBase := new(big.Int).Set(base)
	currentExp := new(big.Int).Set(exp)

	iterations := 0
	for currentExp.Cmp(big.NewInt(0)) > 0 {
		iterations++
		if new(big.Int).And(currentExp, big.NewInt(1)).Cmp(big.NewInt(1)) == 0 {
			result.Mul(result, currentBase)
			result.Div(result, scale)
		}

		currentBase = new(big.Int).Mul(currentBase, currentBase)
		currentBase.Div(currentBase, scale)
		currentExp.Rsh(currentExp, 1)
	}

	return result
}

func benchmark(name string, base *big.Int, exp *big.Int, scale *big.Int, iterations int) {
	fmt.Printf("\n=== %s ===\n", name)
	fmt.Printf("Exponent: %s (bits: %d)\n", exp.String(), exp.BitLen())

	// Warm up
	for i := 0; i < 100; i++ {
		powerWithScale(base, exp, scale)
	}

	// Actual benchmark
	start := time.Now()
	for i := 0; i < iterations; i++ {
		powerWithScale(base, exp, scale)
	}
	elapsed := time.Since(start)

	avgTime := elapsed / time.Duration(iterations)
	fmt.Printf("Total iterations: %d\n", iterations)
	fmt.Printf("Total time: %v\n", elapsed)
	fmt.Printf("Average time per call: %v\n", avgTime)
	fmt.Printf("Operations per second: %.2f\n", float64(iterations)/elapsed.Seconds())
}

func analyzeBitOperations(exp *big.Int) (int, int) {
	bits := exp.BitLen()
	onesCount := 0

	tempExp := new(big.Int).Set(exp)
	for tempExp.Cmp(big.NewInt(0)) > 0 {
		if new(big.Int).And(tempExp, big.NewInt(1)).Cmp(big.NewInt(1)) == 0 {
			onesCount++
		}
		tempExp.Rsh(tempExp, 1)
	}

	return bits, onesCount
}

func main() {
	fmt.Println("=== Fast Exponentiation Performance Benchmark ===\n")

	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(13), nil)
	fmt.Printf("Scale factor: 10^13 = %s\n", scale.String())

	// Calculate realistic base for 5% annual rate
	// ratePerBlock = 0.05 / 10000 / 10512000
	// base = (1 + ratePerBlock) * scale ≈ 1.0000000047619 * scale
	ratePerBlockScaled := new(big.Int).Mul(big.NewInt(500), scale)
	ratePerBlockScaled.Div(ratePerBlockScaled, big.NewInt(10000))
	ratePerBlockScaled.Div(ratePerBlockScaled, big.NewInt(10512000))
	base := new(big.Int).Add(scale, ratePerBlockScaled)

	baseFloat := new(big.Float).Quo(new(big.Float).SetInt(base), new(big.Float).SetInt(scale))
	fmt.Printf("Base (1 + ratePerBlock): %s\n", baseFloat.Text('f', 15))

	// Test different exponents
	testCases := []struct {
		exp        *big.Int
		iterations int
		desc       string
	}{
		{big.NewInt(10), 100000, "Small exponent (10)"},
		{big.NewInt(100), 50000, "Medium exponent (100)"},
		{big.NewInt(1000), 10000, "Large exponent (1000)"},
		{big.NewInt(28800), 5000, "Epoch 28800 (1 day)"},
		{big.NewInt(115200), 2000, "Epoch 115200 (4 days)"},
	}

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("Theoretical Complexity Analysis:")
	fmt.Println(strings.Repeat("=", 60))

	for _, tc := range testCases {
		bits, onesCount := analyzeBitOperations(tc.exp)
		squareOps := bits
		multiplyOps := onesCount
		totalOps := squareOps + multiplyOps

		fmt.Printf("\n%s:\n", tc.desc)
		fmt.Printf("  Exponent value: %s\n", tc.exp.String())
		fmt.Printf("  Binary length: %d bits\n", bits)
		fmt.Printf("  Number of 1-bits: %d\n", onesCount)
		fmt.Printf("  Square operations: %d\n", squareOps)
		fmt.Printf("  Multiply operations: %d\n", multiplyOps)
		fmt.Printf("  Total big.Int operations: %d multiplications + %d divisions\n", totalOps, totalOps)
	}

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("Performance Benchmarks:")
	fmt.Println(strings.Repeat("=", 60))

	for _, tc := range testCases {
		benchmark(tc.desc, base, tc.exp, scale, tc.iterations)
	}

	// Single call timing for accuracy
	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("Single Call Timing (High Precision):")
	fmt.Println(strings.Repeat("=", 60))

	// Test epoch 28800
	fmt.Println("\nEpoch 28800 (typical 1-day epoch):")
	samples := make([]time.Duration, 1000)
	for i := 0; i < 1000; i++ {
		start := time.Now()
		powerWithScale(base, big.NewInt(28800), scale)
		samples[i] = time.Since(start)
	}

	// Calculate statistics
	var total time.Duration
	min := samples[0]
	max := samples[0]
	for _, d := range samples {
		total += d
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
	}
	avg := total / time.Duration(len(samples))

	fmt.Printf("  Samples: %d\n", len(samples))
	fmt.Printf("  Average: %v\n", avg)
	fmt.Printf("  Min: %v\n", min)
	fmt.Printf("  Max: %v\n", max)
	fmt.Printf("  Per validator (if 45 validators): %v total\n", avg*45)

	// Test epoch 115200
	fmt.Println("\nEpoch 115200 (4-day epoch):")
	samples2 := make([]time.Duration, 1000)
	for i := 0; i < 1000; i++ {
		start := time.Now()
		powerWithScale(base, big.NewInt(115200), scale)
		samples2[i] = time.Since(start)
	}

	total2 := time.Duration(0)
	min2 := samples2[0]
	max2 := samples2[0]
	for _, d := range samples2 {
		total2 += d
		if d < min2 {
			min2 = d
		}
		if d > max2 {
			max2 = d
		}
	}
	avg2 := total2 / time.Duration(len(samples2))

	fmt.Printf("  Samples: %d\n", len(samples2))
	fmt.Printf("  Average: %v\n", avg2)
	fmt.Printf("  Min: %v\n", min2)
	fmt.Printf("  Max: %v\n", max2)
	fmt.Printf("  Per validator (if 45 validators): %v total\n", avg2*45)

	fmt.Println("\n" + strings.Repeat("=", 60))
	fmt.Println("Conclusion:")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("\nFor a typical blockchain with 45 validators:\n")
	fmt.Printf("  - Each validator calculates 2 rewards (basic + contribution)\n")
	fmt.Printf("  - Total power operations per breathe block: 90 (45 * 2)\n")
	fmt.Printf("  - With epoch 28800: ~%v per breathe block\n", avg*90)
	fmt.Printf("  - With epoch 115200: ~%v per breathe block\n", avg2*90)
	fmt.Println("\nThis is negligible compared to other blockchain operations.")
}
