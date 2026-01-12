#!/usr/bin/env python3
"""
Generate test data for CalculateContributionRewardRate function.
This script mimics the exact logic of Go's big.Int calculations,
including integer division truncation behavior.
"""


def sqrt_big_int(n):
    """
    Calculate the integer square root using Newton's method.
    This exactly mimics the Go implementation in sqrtBigInt function.
    
    :param n: Number to take square root of (scaled by 10^18), as integer
    :return: Square root (scaled by 10^9), as integer
    """
    if n <= 0:
        return 0
    
    # Initial guess: start with n/2
    x = n // 2
    if x == 0:
        return 1
    
    # Newton's method: x_new = (x + n/x) / 2
    while True:
        # Calculate n/x
        n_div_x = n // x
        
        # Calculate (x + n/x) / 2
        x_new = (x + n_div_x) // 2
        
        # Check convergence
        diff = abs(x - x_new)
        if diff <= 1:
            return x_new
        
        x = x_new


def calculate_contribution_reward_rate(
    inflation_rate,
    in_turn_counts,
    total_turn_counts,
    commission_rate,
    total_delegated,
    total_pooled,
    validators_total_pooled,
    total_supply
):
    """
    Calculate contribution reward rate using the same logic as Go's CalculateContributionRewardRate function.
    All parameters and return values are integers.
    
    :param inflation_rate: Inflation rate in basis points (e.g., 500 = 5%)
    :param in_turn_counts: Number of in-turn blocks
    :param total_turn_counts: Total number of blocks
    :param commission_rate: Commission rate in basis points (e.g., 1000 = 10%)
    :param total_delegated: Total delegated amount (scaled by 10^18)
    :param total_pooled: Total pooled amount (scaled by 10^18)
    :param validators_total_pooled: Total pooled by all validators (scaled by 10^18)
    :param total_supply: Total supply (scaled by 10^18)
    :return: Contribution reward rate in basis points, as integer
    """
    scale = 10 ** 18
    scale_sqrt = 10 ** 9
    basis_point_scale = 10000
    
    # 1. Calculate uptimeRate = inTurnCounts / totalTurnCounts (scaled by 10^18)
    if total_turn_counts > 0:
        uptime_rate_scaled = (in_turn_counts * scale) // total_turn_counts
    else:
        uptime_rate_scaled = 0
    
    # 2. Calculate (1 - commissionRate) where commissionRate is in basis points
    one_minus_commission = basis_point_scale - commission_rate
    one_minus_commission_scaled = (one_minus_commission * scale) // basis_point_scale
    
    # 3. Calculate contributionStakingRatio = totalDelegated / totalPooled (scaled)
    contribution_staking_ratio_scaled = (total_delegated * scale) // total_pooled
    
    # 4. Calculate sqrt(contributionStakingRatio)
    # Since ratio is scaled by 10^18, sqrt will be scaled by 10^9
    sqrt_contrib_ratio = sqrt_big_int(contribution_staking_ratio_scaled)
    
    # 5. Calculate totalNetworkStakingRatio = validatorsTotalPooled / totalSupply (scaled)
    network_staking_ratio_scaled = (validators_total_pooled * scale) // total_supply
    
    # 6. Now calculate: inflationRate * uptimeRate * (1 - commissionRate) / totalNetworkStakingRatio * sqrt(contributionStakingRatio)
    # Start with inflationRate (in basis points)
    result = inflation_rate * scale
    
    # result = inflationRate * uptimeRateScaled / scale
    result = (result * uptime_rate_scaled) // scale
    
    # result = result * oneMinusCommissionScaled / scale
    result = (result * one_minus_commission_scaled) // scale
    
    # result = result * sqrtContribRatio / scaleSqrt
    result = (result * sqrt_contrib_ratio) // scale_sqrt
    
    # result = result * scale / networkStakingRatioScaled (division)
    result = (result * scale) // network_staking_ratio_scaled
    
    # Result is now in basis points
    return result


def format_go_big_int(value):
    """
    Format a large integer for Go test code.
    """
    if value == 0:
        return "big.NewInt(0)"
    
    value_str = str(value)
    
    # For small numbers, use big.NewInt
    if value < 2**63 - 1:  # int64 max
        return f"big.NewInt({value})"
    
    # For large numbers, use SetString
    return f'mustBigIntFromString("{value}")'


def generate_test_cases():
    """
    Generate comprehensive test cases including edge cases.
    """
    scale = 10 ** 18
    
    test_cases = [
        # Basic test cases
        {
            "name": "Basic contribution reward calculation",
            "inflation_rate": 500,  # 5%
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,  # 10%
            "total_delegated": scale,  # 1 token
            "total_pooled": 10 * scale,  # 10 tokens
            "validators_total_pooled": 100 * scale,  # 100 tokens
            "total_supply": 1000 * scale,  # 1000 tokens
        },
        {
            "name": "Perfect uptime (100%)",
            "inflation_rate": 500,
            "in_turn_counts": 10000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "High inflation rate (10%)",
            "inflation_rate": 1000,  # 10%
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Zero commission rate",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 0,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "High commission rate (50%)",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 5000,  # 50%
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Maximum commission rate (100%)",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 10000,  # 100%
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        
        # Zero and edge cases
        {
            "name": "Zero inflation rate",
            "inflation_rate": 0,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Zero uptime",
            "inflation_rate": 500,
            "in_turn_counts": 0,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Zero total turn counts",
            "inflation_rate": 500,
            "in_turn_counts": 0,
            "total_turn_counts": 0,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Zero total delegated",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": 0,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Equal total delegated and pooled",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": scale,  # Same as delegated
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        
        # Large value cases
        {
            "name": "Large total delegated",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": 1000000 * scale,  # 1M tokens
            "total_pooled": 10000000 * scale,  # 10M tokens
            "validators_total_pooled": 100000000 * scale,  # 100M tokens
            "total_supply": 1000000000 * scale,  # 1B tokens
        },
        {
            "name": "Very small contribution ratio",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,  # 1 token
            "total_pooled": 1000000 * scale,  # 1M tokens
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "Very small network staking ratio",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": scale,  # 1 token
            "total_supply": 1000000 * scale,  # 1M tokens
        },
        
        # Edge cases for ratios
        {
            "name": "High contribution ratio (50%)",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": 5 * scale,
            "total_pooled": 10 * scale,  # 50% ratio
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "High network staking ratio (50%)",
            "inflation_rate": 500,
            "in_turn_counts": 8000,
            "total_turn_counts": 10000,
            "commission_rate": 1000,
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 500 * scale,  # 50% of supply
            "total_supply": 1000 * scale,
        },
        
        # Combined edge cases
        {
            "name": "Low uptime with high commission",
            "inflation_rate": 500,
            "in_turn_counts": 1000,  # 10% uptime
            "total_turn_counts": 10000,
            "commission_rate": 5000,  # 50% commission
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
        {
            "name": "High inflation with perfect uptime and zero commission",
            "inflation_rate": 1000,  # 10%
            "in_turn_counts": 10000,  # 100% uptime
            "total_turn_counts": 10000,
            "commission_rate": 0,  # 0% commission
            "total_delegated": scale,
            "total_pooled": 10 * scale,
            "validators_total_pooled": 100 * scale,
            "total_supply": 1000 * scale,
        },
    ]
    
    return test_cases


def main():
    """
    Main function to generate and display test case results.
    """
    print("=" * 80)
    print("CalculateContributionRewardRate Test Data Generator")
    print("=" * 80)
    print()
    
    test_cases = generate_test_cases()
    scale = 10 ** 18
    
    results = []
    
    for i, tc in enumerate(test_cases, 1):
        print(f"Test Case {i}: {tc['name']}")
        print("-" * 80)
        
        print(f"Input parameters:")
        print(f"  inflationRate:         {tc['inflation_rate']} (basis points)")
        print(f"  inTurnCounts:          {tc['in_turn_counts']}")
        print(f"  totalTurnCounts:       {tc['total_turn_counts']}")
        print(f"  commissionRate:        {tc['commission_rate']} (basis points)")
        print(f"  totalDelegated:        {tc['total_delegated']}")
        print(f"  totalPooled:            {tc['total_pooled']}")
        print(f"  validatorsTotalPooled: {tc['validators_total_pooled']}")
        print(f"  totalSupply:           {tc['total_supply']}")
        print()
        
        # Calculate reward rate
        try:
            result = calculate_contribution_reward_rate(
                tc['inflation_rate'],
                tc['in_turn_counts'],
                tc['total_turn_counts'],
                tc['commission_rate'],
                tc['total_delegated'],
                tc['total_pooled'],
                tc['validators_total_pooled'],
                tc['total_supply']
            )
            
            print(f"Result:")
            print(f"  contributionRewardRate: {result} (basis points)")
            print(f"  contributionRewardRate (Go format): {format_go_big_int(result)}")
            print()
            
            results.append({
                "name": tc['name'],
                "result": result,
                "go_format": format_go_big_int(result),
                "test_case": tc
            })
        except Exception as e:
            print(f"Error calculating reward rate: {e}")
            import traceback
            traceback.print_exc()
            print()
            results.append({
                "name": tc['name'],
                "result": None,
                "go_format": "// Error in calculation",
                "test_case": tc
            })
    
    print("=" * 80)
    print("Go Test Code")
    print("=" * 80)
    print()
    print("Copy this into your test file:")
    print()
    
    for result in results:
        tc = result['test_case']
        if result['result'] is None:
            print(f"\t\t// {result['name']} - SKIPPED (calculation error)")
            continue
        
        # Format large integers
        def format_token(value):
            if value == 0:
                return "big.NewInt(0)"
            tokens = value // scale
            if tokens == 0:
                return "big.NewInt(0)"
            elif tokens == 1:
                return "new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1 token"
            else:
                return f"new(big.Int).Mul(big.NewInt({tokens}), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)) // {tokens} tokens"
        
        print(f"\t\t{{")
        print(f"\t\t\tname:                  \"{tc['name']}\",")
        print(f"\t\t\tinflationRate:         big.NewInt({tc['inflation_rate']}), // {tc['inflation_rate']/100}% in basis points")
        print(f"\t\t\tinTurnCounts:           big.NewInt({tc['in_turn_counts']}),")
        print(f"\t\t\ttotalTurnCounts:       big.NewInt({tc['total_turn_counts']}),")
        print(f"\t\t\tcommissionRate:        big.NewInt({tc['commission_rate']}), // {tc['commission_rate']/100}% in basis points")
        print(f"\t\t\ttotalDelegated:        {format_token(tc['total_delegated'])},")
        print(f"\t\t\ttotalPooled:           {format_token(tc['total_pooled'])},")
        print(f"\t\t\tvalidatorsTotalPooled: {format_token(tc['validators_total_pooled'])},")
        print(f"\t\t\ttotalSupply:           {format_token(tc['total_supply'])},")
        print(f"\t\t\texpected:              {result['go_format']},")
        print(f"\t\t}},")
    
    print()
    print("=" * 80)


if __name__ == "__main__":
    main()

