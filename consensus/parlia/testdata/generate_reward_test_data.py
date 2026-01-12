#!/usr/bin/env python3
"""
Generate test data for CalculateRewardByRate function.
This script mimics the exact logic of Go's big.Int calculations,
including integer division truncation behavior.
"""

def power_with_scale(base, exp, scale):
    """
    Calculate base^exp using fast exponentiation with scale.
    This exactly mimics the Go implementation in powerWithScale function.
    
    :param base: Base value (already scaled by scale), as integer
    :param exp: Exponent, as integer
    :param scale: Scale factor (10^18), as integer
    :return: Result scaled by scale, as integer
    """
    if exp == 0:
        return scale
    
    # Initialize result to 1 * scale
    result = scale
    current_base = base
    current_exp = exp
    
    # Fast exponentiation algorithm (binary exponentiation)
    while current_exp > 0:
        # If current exponent is odd, multiply result by currentBase
        if current_exp & 1 == 1:
            # result = (result * currentBase) / scale
            # Using integer division (//) to match Go's big.Int.Div() truncation
            result = (result * current_base) // scale
        
        # Square the base: currentBase = (currentBase * currentBase) / scale
        # Using integer division to match Go's big.Int.Div() truncation
        current_base = (current_base * current_base) // scale
        
        # Divide exponent by 2 (right shift by 1 bit)
        current_exp = current_exp >> 1
    
    return result


def calculate_reward_by_rate(rate, annual_block_count_year, annual_block_count_epoch, total_pooled):
    """
    Calculate reward using the same logic as Go's CalculateRewardByRate function.
    All parameters and return values are integers (wei scale).
    
    :param rate: Interest rate scaled by 10^18 (rate in basis points * 10^18)
    :param annual_block_count_year: Number of blocks per year, as integer
    :param annual_block_count_epoch: Number of blocks per epoch, as integer
    :param total_pooled: Total pooled amount (scaled by 10^18), as integer
    :return: Reward amount (scaled by 10^18), as integer
    """
    scale = 10 ** 18
    
    # ratePerBlock = rate / 10000 / annualBlockCountEveryYear
    # ratePerBlockScaled = rate / annualBlockCountEveryYear / 10000
    # Note: Go's big.Int.Div() truncates toward zero (integer division)
    rate_per_block_scaled = rate // annual_block_count_year
    rate_per_block_scaled = rate_per_block_scaled // 10000
    
    # base = (1 + ratePerBlock) * scale = scale + ratePerBlockScaled
    base = scale + rate_per_block_scaled
    
    # Use fast exponentiation to calculate base^annualBlocksEpoch
    result = power_with_scale(base, annual_block_count_epoch, scale)
    
    # result is now (1 + ratePerBlock)^annualBlocksEpoch * scale
    # compoundRate = result - scale (this is the actual rate multiplied by scale)
    compound_rate = result - scale
    
    # reward = totalPooled * compoundRate / scale
    reward = (total_pooled * compound_rate) // scale
    
    return reward


def format_go_big_int(value):
    """
    Format a large integer for Go test code.
    """
    if value == 0:
        return "big.NewInt(0)"
    
    value_str = str(value)
    
    # For small numbers, use big.NewInt
    if value < 10**18:
        return f"big.NewInt({value})"
    
    # For large numbers, try to format as: big.NewInt(part) * 10^exp
    # Find a good split point (keep at least 5 digits in the integer part)
    for split in range(len(value_str) - 10, len(value_str) - 5, -1):
        if split > 0:
            integer_part = int(value_str[:split])
            exponent = len(value_str) - split
            if integer_part > 0 and exponent > 0:
                return f"new(big.Int).Mul(big.NewInt({integer_part}), new(big.Int).Exp(big.NewInt(10), big.NewInt({exponent}), nil))"
    
    # Fallback: use big.NewInt with the full number
    return f"big.NewInt({value})"


def generate_test_cases():
    """
    Generate comprehensive test cases including edge cases.
    """
    scale = 10 ** 18
    
    test_cases = [
        # Basic test cases
        {
            "name": "Basic reward calculation with 2% rate",
            "rate_basis_points": 200,
            "annual_block_count_year": 42048000,
            "annual_block_count_epoch": 115200,
            "total_pooled": scale,
        },
        {
            "name": "Basic reward calculation with 5% rate",
            "rate_basis_points": 500,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        {
            "name": "Basic reward calculation with 10% rate",
            "rate_basis_points": 1000,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        
        # Zero and minimum cases
        {
            "name": "Zero rate should return zero reward",
            "rate_basis_points": 0,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        {
            "name": "Minimum rate (1 basis point)",
            "rate_basis_points": 1,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        {
            "name": "Zero total pooled",
            "rate_basis_points": 200,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": 0,
        },
        
        # Maximum rate cases
        {
            "name": "Maximum inflation rate (500 basis points = 5%)",
            "rate_basis_points": 500,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        {
            "name": "High rate (1000 basis points = 10%)",
            "rate_basis_points": 1000,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        {
            "name": "Very high rate (5000 basis points = 50%)",
            "rate_basis_points": 5000,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": scale,
        },
        
        # Large total pooled cases
        {
            "name": "Large total pooled (1M tokens)",
            "rate_basis_points": 500,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": 1000000 * scale,
        },
        {
            "name": "Very large total pooled (1B tokens)",
            "rate_basis_points": 500,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": 1000000000 * scale,
        },
        {
            "name": "Extremely large total pooled (1T tokens)",
            "rate_basis_points": 200,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": 1000000000000 * scale,
        },
        
        # Short epoch cases
        {
            "name": "Short epoch (1 hour)",
            "rate_basis_points": 200,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 3600,  # 1 hour
            "total_pooled": scale,
        },
        {
            "name": "Very short epoch (1 block)",
            "rate_basis_points": 200,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 1,
            "total_pooled": scale,
        },
        
        # Long epoch cases
        {
            "name": "Long epoch (1 week)",
            "rate_basis_points": 200,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 604800,  # 1 week
            "total_pooled": scale,
        },
        {
            "name": "Very long epoch (1 month)",
            "rate_basis_points": 200,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 2592000,  # 1 month
            "total_pooled": scale,
        },
        
        # Edge cases for block counts
        {
            "name": "Small annual block count",
            "rate_basis_points": 200,
            "annual_block_count_year": 1000,
            "annual_block_count_epoch": 100,
            "total_pooled": scale,
        },
        {
            "name": "Very large annual block count",
            "rate_basis_points": 200,
            "annual_block_count_year": 100000000,
            "annual_block_count_epoch": 1000000,
            "total_pooled": scale,
        },
        
        # Combined edge cases
        {
            "name": "High rate with large total pooled",
            "rate_basis_points": 1000,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 86400,
            "total_pooled": 1000000 * scale,
        },
        {
            "name": "Low rate with small epoch",
            "rate_basis_points": 1,
            "annual_block_count_year": 31536000,
            "annual_block_count_epoch": 1,
            "total_pooled": scale,
        },
    ]
    
    return test_cases


def main():
    """
    Main function to generate and display test case results.
    """
    print("=" * 80)
    print("CalculateRewardByRate Test Data Generator")
    print("=" * 80)
    print()
    
    test_cases = generate_test_cases()
    scale = 10 ** 18
    
    results = []
    
    for i, tc in enumerate(test_cases, 1):
        print(f"Test Case {i}: {tc['name']}")
        print("-" * 80)
        
        # Scale the rate: rateScaled = rate * scale
        rate_scaled = tc['rate_basis_points'] * scale
        
        print(f"Input parameters:")
        print(f"  rate (basis points):           {tc['rate_basis_points']}")
        print(f"  annualBlockCountEveryYear:    {tc['annual_block_count_year']}")
        print(f"  annualBlockCountEveryEpoch:   {tc['annual_block_count_epoch']}")
        print(f"  totalPooled:                  {tc['total_pooled']}")
        print()
        
        # Calculate reward
        try:
            reward = calculate_reward_by_rate(
                rate_scaled,
                tc['annual_block_count_year'],
                tc['annual_block_count_epoch'],
                tc['total_pooled']
            )
            
            print(f"Result:")
            print(f"  reward:                      {reward}")
            print(f"  reward (Go format):          {format_go_big_int(reward)}")
            print()
            
            results.append({
                "name": tc['name'],
                "reward": reward,
                "go_format": format_go_big_int(reward),
                "test_case": tc
            })
        except Exception as e:
            print(f"Error calculating reward: {e}")
            print()
            results.append({
                "name": tc['name'],
                "reward": None,
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
        if result['reward'] is None:
            print(f"\t\t// {result['name']} - SKIPPED (calculation error)")
            continue
        
        # Format totalPooled
        if tc['total_pooled'] == scale:
            total_pooled_str = "new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) // 1 token"
        elif tc['total_pooled'] == 0:
            total_pooled_str = "big.NewInt(0)"
        else:
            tokens = tc['total_pooled'] // scale
            total_pooled_str = f"new(big.Int).Mul(big.NewInt({tokens}), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)) // {tokens} tokens"
        
        print(f"\t\t{{")
        print(f"\t\t\tname:                       \"{tc['name']}\",")
        print(f"\t\t\trate:                       big.NewInt({tc['rate_basis_points']}), // {tc['rate_basis_points']/100}% in basis points")
        print(f"\t\t\tannualBlockCountEveryYear:  big.NewInt({tc['annual_block_count_year']}),")
        print(f"\t\t\tannualBlockCountEveryEpoch:  big.NewInt({tc['annual_block_count_epoch']}),")
        print(f"\t\t\ttotalPooled:                {total_pooled_str},")
        print(f"\t\t\texpectedBasicReward:        {result['go_format']},")
        print(f"\t\t}},")
    
    print()
    print("=" * 80)


if __name__ == "__main__":
    main()

