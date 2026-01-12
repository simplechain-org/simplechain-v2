#!/usr/bin/env python3
"""
Generate test data for calculateNewYearInflation function.
This script mimics the exact logic of Go's big.Int calculations,
including integer division truncation behavior.
"""

MAX_INFLATION_RATE = 500  # 500 basis points


def calculate_new_year_inflation(current_total_supply, last_year_total_supply):
    """
    Calculate new year inflation using the same logic as Go's calculateNewYearInflation function.
    All parameters and return values are integers (wei scale).
    
    :param current_total_supply: Current total supply (scaled by 10^18), as integer
    :param last_year_total_supply: Last year's total supply (scaled by 10^18), as integer
    :return: (additional_issuance_amount, new_inflation_rate) where rate is in basis points
    """
    scale = 10 ** 18
    
    # Calculate additional issuance amount
    additional_issuance_amount = current_total_supply - last_year_total_supply
    
    # If <= 0, return (0, 0)
    if additional_issuance_amount <= 0:
        return 0, 0
    
    # Calculate new inflation rate
    # newInflationRate = (additionalIssuanceAmount * scale * 10000) / currentTotalSupply / scale
    additional_issuance_scaled = additional_issuance_amount * scale
    additional_issuance_scaled = additional_issuance_scaled * 10000
    
    # Using integer division to match Go's big.Int.Div() truncation
    new_inflation_rate = additional_issuance_scaled // current_total_supply
    new_inflation_rate = new_inflation_rate // scale
    
    # Cap at MaxInflationRate
    if new_inflation_rate > MAX_INFLATION_RATE:
        new_inflation_rate = MAX_INFLATION_RATE
    
    return additional_issuance_amount, new_inflation_rate


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
            "name": "Normal inflation calculation (10x growth)",
            "current_total_supply": 1000 * scale,  # 1000 tokens
            "last_year_total_supply": 100 * scale,  # 100 tokens
        },
        {
            "name": "Normal inflation calculation (2% growth)",
            "current_total_supply": 1020 * scale,  # 1020 tokens
            "last_year_total_supply": 1000 * scale,  # 1000 tokens
        },
        {
            "name": "Normal inflation calculation (5% growth)",
            "current_total_supply": 1050 * scale,  # 1050 tokens
            "last_year_total_supply": 1000 * scale,  # 1000 tokens
        },
        {
            "name": "Normal inflation calculation (10% growth)",
            "current_total_supply": 1100 * scale,  # 1100 tokens
            "last_year_total_supply": 1000 * scale,  # 1000 tokens
        },
        
        # Zero and edge cases
        {
            "name": "No inflation (same supply)",
            "current_total_supply": 1000 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Negative inflation (deflation)",
            "current_total_supply": 100 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Zero current supply",
            "current_total_supply": 0,
            "last_year_total_supply": 100 * scale,
        },
        {
            "name": "Zero last year supply",
            "current_total_supply": 100 * scale,
            "last_year_total_supply": 0,
        },
        {
            "name": "Both zero",
            "current_total_supply": 0,
            "last_year_total_supply": 0,
        },
        
        # Small differences
        {
            "name": "Very small inflation (1 wei)",
            "current_total_supply": 1000 * scale + 1,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Small inflation (1 token)",
            "current_total_supply": 1001 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Small inflation (0.1% growth)",
            "current_total_supply": 1001 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        
        # High inflation cases
        {
            "name": "High inflation (50% growth)",
            "current_total_supply": 1500 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Very high inflation (100% growth)",
            "current_total_supply": 2000 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Extremely high inflation (1000% growth)",
            "current_total_supply": 11000 * scale,
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Inflation exceeding MaxInflationRate",
            "current_total_supply": 2000 * scale,  # 100% growth = 10000 basis points
            "last_year_total_supply": 1000 * scale,
        },
        {
            "name": "Inflation exactly at MaxInflationRate",
            "current_total_supply": 1050 * scale,  # 5% growth = 500 basis points
            "last_year_total_supply": 1000 * scale,
        },
        
        # Large value cases
        {
            "name": "Large total supply (1B tokens)",
            "current_total_supply": 1000000000 * scale,
            "last_year_total_supply": 100000000 * scale,
        },
        {
            "name": "Very large total supply (1T tokens)",
            "current_total_supply": 1000000000000 * scale,
            "last_year_total_supply": 100000000000 * scale,
        },
        {
            "name": "Large supply with small difference",
            "current_total_supply": 1000000000 * scale + scale,
            "last_year_total_supply": 1000000000 * scale,
        },
        
        # Edge cases for rate calculation
        {
            "name": "Rate calculation precision test",
            "current_total_supply": 1000000 * scale,
            "last_year_total_supply": 999999 * scale,
        },
        {
            "name": "Rate calculation with rounding",
            "current_total_supply": 1000000 * scale,
            "last_year_total_supply": 999500 * scale,
        },
    ]
    
    return test_cases


def main():
    """
    Main function to generate and display test case results.
    """
    print("=" * 80)
    print("calculateNewYearInflation Test Data Generator")
    print("=" * 80)
    print()
    
    test_cases = generate_test_cases()
    scale = 10 ** 18
    
    results = []
    
    for i, tc in enumerate(test_cases, 1):
        print(f"Test Case {i}: {tc['name']}")
        print("-" * 80)
        
        print(f"Input parameters:")
        current_tokens = tc['current_total_supply'] // scale
        last_tokens = tc['last_year_total_supply'] // scale
        print(f"  currentTotalSupply:    {tc['current_total_supply']} ({current_tokens} tokens)")
        print(f"  lastYearTotalSupply:   {tc['last_year_total_supply']} ({last_tokens} tokens)")
        print()
        
        # Calculate inflation
        try:
            additional_issuance, new_inflation_rate = calculate_new_year_inflation(
                tc['current_total_supply'],
                tc['last_year_total_supply']
            )
            
            print(f"Result:")
            print(f"  additionalIssuanceAmount: {additional_issuance}")
            print(f"  newInflationRate:         {new_inflation_rate} (basis points = {new_inflation_rate/100}%)")
            print()
            
            results.append({
                "name": tc['name'],
                "additional_issuance": additional_issuance,
                "new_inflation_rate": new_inflation_rate,
                "go_format_issuance": format_go_big_int(additional_issuance),
                "go_format_rate": format_go_big_int(new_inflation_rate),
                "test_case": tc
            })
        except Exception as e:
            print(f"Error calculating inflation: {e}")
            import traceback
            traceback.print_exc()
            print()
            results.append({
                "name": tc['name'],
                "additional_issuance": None,
                "new_inflation_rate": None,
                "go_format_issuance": "// Error in calculation",
                "go_format_rate": "// Error in calculation",
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
        if result['additional_issuance'] is None:
            print(f"\t\t// {result['name']} - SKIPPED (calculation error)")
            continue
        
        # Format total supply values
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
        
        current_tokens = tc['current_total_supply'] // scale
        last_tokens = tc['last_year_total_supply'] // scale
        
        # Handle very large numbers
        if current_tokens > 2**63 - 1:
            current_supply_str = f'mustBigIntFromString("{tc["current_total_supply"]}")'
        else:
            current_supply_str = format_token(tc['current_total_supply'])
        
        if last_tokens > 2**63 - 1:
            last_supply_str = f'mustBigIntFromString("{tc["last_year_total_supply"]}")'
        else:
            last_supply_str = format_token(tc['last_year_total_supply'])
        
        print(f"\t\t{{")
        print(f"\t\t\tname:                \"{tc['name']}\",")
        print(f"\t\t\tcurrentTotalSupply:  {current_supply_str},")
        print(f"\t\t\tlastYearTotalSupply: {last_supply_str},")
        print(f"\t\t\texpectedIssuance:    {result['go_format_issuance']},")
        print(f"\t\t\texpectedRate:       {result['go_format_rate']},")
        print(f"\t\t}},")
    
    print()
    print("=" * 80)


if __name__ == "__main__":
    main()

