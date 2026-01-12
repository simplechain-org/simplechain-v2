package parlia

import (
	"context"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

const MaxInflationRate = 500 // 500 basis points

type CommissionItem struct {
	Rate          uint64
	MaxRate       uint64
	MaxChangeRate uint64
}

// ethAPIReader defines the interface for reading data from ethAPI.
// This abstraction makes it easier to test functions that depend on blockchain state.
type ethAPIReader interface {
	GetNominalInterestRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error)
	GetInflationRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error)
	GetMaxContributionRewardRatio(blockNr rpc.BlockNumberOrHash) (*big.Int, error)
	GetTotalSupply(blockNr rpc.BlockNumberOrHash) (*big.Int, *big.Int, error)
	GetValidatorsTotalPooled(blockNr rpc.BlockNumberOrHash) (*big.Int, error)
	GetAllValidators(blockNr rpc.BlockNumberOrHash) ([]common.Address, error)
	GetValidatorDelegatedAmount(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*big.Int, *big.Int, *big.Int, error)
	GetValidatorConsensusAddress(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (common.Address, error)
	GetValidatorUptimeRecord(blockNr rpc.BlockNumberOrHash, val common.Address, index *big.Int) (*big.Int, *big.Int, error)
	GetValidatorCommissionRate(blockNr rpc.BlockNumberOrHash, val common.Address) (CommissionItem, error)
}

// ethAPIWriter defines the interface for writing data to state database.
// This abstraction makes it easier to test functions that modify blockchain state.
type ethAPIWriter interface {
	SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason)
	AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason)
	GetBalance(addr common.Address) *uint256.Int
	DistributeIncoming(val common.Address, state vm.StateDB, header *types.Header, chain core.ChainContext,
		txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) (*big.Int, error)
}

// ValidatorRewardInput contains all input data needed to calculate validator rewards
// This struct is used to separate data fetching from calculation logic for easier testing
type ValidatorRewardInput struct {
	// Basic reward calculation inputs
	NominalInterestRateScaled  *big.Int // Scaled by 10^18
	AnnualBlockCountEveryYear  *big.Int
	AnnualBlockCountEveryEpoch *big.Int
	TotalPooled                *big.Int

	// Contribution reward calculation inputs
	InflationRate              *big.Int
	InTurnCounts               *big.Int
	TotalTurnCounts            *big.Int
	CommissionRate             *big.Int // In basis points
	TotalDelegated             *big.Int
	ValidatorsTotalPooled      *big.Int
	TotalSupply                *big.Int
	MaxContributionRewardRatio *big.Int // Scaled by 10^18
}

// ValidatorRewardResult contains the calculated reward results
type ValidatorRewardResult struct {
	BasicReward        *big.Int
	ContributionReward *big.Int
}

// validatorRewardData contains validator-specific data fetched from blockchain state
type validatorRewardData struct {
	TotalDelegated   *big.Int
	TotalPooled      *big.Int
	SelfDelegated    *big.Int
	ConsensusAddress common.Address
	InTurnCounts     *big.Int
	OutTurnCounts    *big.Int
	TotalTurnCounts  *big.Int
	CommissionRate   *big.Int
}

func (p *Parlia) getNominalInterestRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	method := "nominalInterestRate"
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for nominalInterestRate", "error", err)
		return nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, err
	}

	return unpacked[0].(*big.Int), nil
}

func (p *Parlia) getValidatorDelegatedInfo(blockNr rpc.BlockNumberOrHash, operatorAddress common.Address) (*big.Int, *big.Int, *big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorDelegatedInfo"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method, operatorAddress)
	if err != nil {
		log.Error("Unable to pack tx for getValidatorDelegatedInfo", "error", err)
		return nil, nil, nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, nil, nil, err
	}

	unpacked, err := p.stakeHubABI.Unpack(method, result)
	if err != nil {
		return nil, nil, nil, err
	}

	return unpacked[0].(*big.Int), unpacked[1].(*big.Int), unpacked[2].(*big.Int), nil
}

func (p *Parlia) getValidatorDelegatedAmount(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*big.Int, *big.Int, *big.Int, error) {
	totalPooled, selfDelegated, otherDelegated, err := p.getValidatorDelegatedInfo(blockNr, operatorAddr)
	if err != nil {
		log.Error("Unable to get total pooled", "error", err)
		return nil, nil, nil, err
	}
	// Minimum amount of delegated required
	minDelegated := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	if otherDelegated.Cmp(minDelegated) < 0 {
		otherDelegated = big.NewInt(0)
	}
	return otherDelegated, totalPooled, selfDelegated, nil
}

func (p *Parlia) getValidatorUptimeRecord(blockNr rpc.BlockNumberOrHash, val common.Address, index *big.Int) (*big.Int, *big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorUptimeRecord"
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.validatorSetABI.Pack(method, val, index)
	if err != nil {
		log.Error("Unable to pack tx for getValidatorUptimeRecord", "error", err)
		return nil, nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, nil, err
	}

	return unpacked[0].(*big.Int), unpacked[1].(*big.Int), nil
}

func (p *Parlia) getTotalSupply(blockNr rpc.BlockNumberOrHash) (*big.Int, *big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getTotalSupply"
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getTotalSupply", "error", err)
		return nil, nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, nil, err
	}

	return unpacked[0].(*big.Int), unpacked[1].(*big.Int), nil
}
func (p *Parlia) getTotalIssuanceAmountOfReward(blockNr rpc.BlockNumberOrHash) (*big.Int, *big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getTotalIssuanceAmountOfReward"
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getTotalIssuanceAmountOfReward", "error", err)
		return nil, nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, nil, err
	}

	return unpacked[0].(*big.Int), unpacked[1].(*big.Int), nil
}
func (p *Parlia) getAllValidators(blockNr rpc.BlockNumberOrHash) ([]common.Address, error) {
	method := "getValidators"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	const pageSize = 50
	var allOperatorAddrs []common.Address
	offset := uint64(0)

	for {
		ctx, cancel := context.WithCancel(context.Background())

		// Pack with offset and limit parameters
		data, err := p.stakeHubABI.Pack(method, new(big.Int).SetUint64(offset), new(big.Int).SetUint64(pageSize))
		if err != nil {
			cancel()
			log.Error("Unable to pack tx for getValidators", "error", err)
			return nil, err
		}
		msgData := (hexutil.Bytes)(data)

		result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
			Gas:  &gas,
			To:   &toAddress,
			Data: &msgData,
		}, &blockNr, nil, nil)
		cancel()

		if err != nil {
			return nil, err
		}

		// Unpack: returns (address[] operatorAddrs, address[] creditAddrs, uint256 totalLength)
		var operatorAddrs []common.Address
		var creditAddrs []common.Address
		var totalLength *big.Int
		err = p.stakeHubABI.UnpackIntoInterface(&[]interface{}{&operatorAddrs, &creditAddrs, &totalLength}, method, result)
		if err != nil {
			return nil, err
		}

		allOperatorAddrs = append(allOperatorAddrs, operatorAddrs...)

		// If we've fetched all validators, break
		if offset+uint64(len(operatorAddrs)) >= totalLength.Uint64() || len(operatorAddrs) == 0 {
			break
		}

		offset += uint64(len(operatorAddrs))
	}

	return allOperatorAddrs, nil
}

func (p *Parlia) getValidatorsTotalPooled(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorsTotalPooled"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getValidatorsTotalPooled", "error", err)
		return nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, err
	}

	unpacked, err := p.stakeHubABI.Unpack(method, result)
	if err != nil {
		return nil, err
	}

	return unpacked[0].(*big.Int), nil
}

func (p *Parlia) getValidatorConsensusAddress(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (common.Address, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorConsensusAddress"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method, operatorAddr)
	if err != nil {
		return common.Address{}, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return common.Address{}, err
	}

	unpacked, err := p.stakeHubABI.Unpack(method, result)
	if err != nil {
		return common.Address{}, err
	}

	return unpacked[0].(common.Address), nil
}

func (p *Parlia) getValidatorCommissionRate(blockNr rpc.BlockNumberOrHash, val common.Address) (CommissionItem, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorCommission"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method, val)
	if err != nil {
		return CommissionItem{}, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return CommissionItem{}, err
	}

	unpacked, err := p.stakeHubABI.Unpack(method, result)
	if err != nil {
		return CommissionItem{}, err
	}

	commission := unpacked[0].(struct {
		Rate          uint64 `json:"rate"`
		MaxRate       uint64 `json:"maxRate"`
		MaxChangeRate uint64 `json:"maxChangeRate"`
	})

	return CommissionItem{
		Rate:          commission.Rate,
		MaxRate:       commission.MaxRate,
		MaxChangeRate: commission.MaxChangeRate,
	}, nil
}

func (p *Parlia) getMaxContributionRewardRatio(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "maxContributionRewardRatio"
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getMaxContributionRewardRatio", "error", err)
		return nil, err
	}
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, err
	}

	return unpacked[0].(*big.Int), nil
}

// CalculateRewardByRateWithBigInt calculates reward using big.Int arithmetic with fast exponentiation
// rate is in basis points (e.g., 500 for 5%)
// Formula: reward = totalPooled * ((1 + rate/10000/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1)
func (p *Parlia) CalculateRewardByRate(rate *big.Int, annualBlockCountEveryYear *big.Int,
	annualBlockCountEveryEpoch *big.Int, totalPooled *big.Int) *big.Int {

	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	// ratePerBlock = rate / 10000 / annualBlockCountEveryYear
	// ratePerBlockScaled = rate * 10000 / annualBlockCountEveryYear
	ratePerBlockScaled := new(big.Int).Set(rate)
	ratePerBlockScaled.Div(ratePerBlockScaled, annualBlockCountEveryYear)
	ratePerBlockScaled.Div(ratePerBlockScaled, big.NewInt(10000))

	// base = (1 + ratePerBlock) * scale = scale + ratePerBlockScaled
	base := new(big.Int).Add(scale, ratePerBlockScaled)
	// Use fast exponentiation to calculate base^annualBlocksEpoch
	// Each multiplication divides by scale to keep the number from growing too large
	result := powerWithScale(base, annualBlockCountEveryEpoch, scale)

	// result is now (1 + ratePerBlock)^annualBlocksEpoch * scale
	// compoundRate = result - scale (this is the actual rate multiplied by scale)
	compoundRate := new(big.Int).Sub(result, scale)

	// reward = totalPooled * compoundRate / scale
	reward := new(big.Int).Mul(totalPooled, compoundRate)
	reward.Div(reward, scale)

	return reward
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

// CalculateContributionRewardRate calculates the contribution reward rate using big.Int arithmetic
// Formula: contributionRewardRate = inflationRate * uptimeRate * (1 - commissionRate) / totalNetworkStakingRatio * sqrt(contributionStakingRatio)
// All rates are in basis points (10000 = 100%)
// Returns: reward rate in basis points
func CalculateContributionRewardRate(
	inflationRate *big.Int, // basis points (e.g., 500 = 5%)
	inTurnCounts *big.Int,
	totalTurnCounts *big.Int,
	commissionRate *big.Int, // basis points (e.g., 1000 = 10%)
	totalDelegated *big.Int,
	totalPooled *big.Int,
	validatorsTotalPooled *big.Int,
	totalSupply *big.Int,
) *big.Int {
	// Scale factor: 10^18 for high precision
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	scaleSqrt := new(big.Int).Exp(big.NewInt(10), big.NewInt(9), nil)
	basisPointScale := big.NewInt(10000)

	// 1. Calculate uptimeRate = inTurnCounts / totalTurnCounts (scaled by 10^18)
	var uptimeRateScaled *big.Int
	if totalTurnCounts.Cmp(big.NewInt(0)) > 0 {
		uptimeRateScaled = new(big.Int).Mul(inTurnCounts, scale)
		uptimeRateScaled.Div(uptimeRateScaled, totalTurnCounts)
	} else {
		return big.NewInt(0)
	}

	// 2. Calculate (1 - commissionRate) where commissionRate is in basis points
	oneMinusCommission := new(big.Int).Sub(basisPointScale, commissionRate)
	oneMinusCommissionScaled := new(big.Int).Mul(oneMinusCommission, scale)
	oneMinusCommissionScaled.Div(oneMinusCommissionScaled, basisPointScale)

	// 3. Calculate contributionStakingRatio = totalDelegated / totalPooled (scaled)
	contributionStakingRatioScaled := new(big.Int).Mul(totalDelegated, scale)
	if totalPooled.Cmp(big.NewInt(0)) > 0 {
		contributionStakingRatioScaled.Div(contributionStakingRatioScaled, totalPooled)
	} else {
		return big.NewInt(0)
	}

	// 4. Calculate sqrt(contributionStakingRatio)
	// Since ratio is scaled by 10^18, sqrt will be scaled by 10^9
	sqrtContribRatio := sqrtBigInt(contributionStakingRatioScaled)

	// 5. Calculate totalNetworkStakingRatio = validatorsTotalPooled / totalSupply (scaled)
	networkStakingRatioScaled := new(big.Int).Mul(validatorsTotalPooled, scale)
	if totalSupply.Cmp(big.NewInt(0)) > 0 {
		networkStakingRatioScaled.Div(networkStakingRatioScaled, totalSupply)
	} else {
		return big.NewInt(0)
	}

	// 6. Now calculate: inflationRate * uptimeRate * (1 - commissionRate) / totalNetworkStakingRatio * sqrt(contributionStakingRatio)
	// Start with inflationRate (in basis points)
	result := new(big.Int).Mul(inflationRate, scale)

	// result = inflationRate * uptimeRateScaled / scale
	result.Mul(result, uptimeRateScaled)
	result.Div(result, scale)

	// result = result * oneMinusCommissionScaled / scale
	result.Mul(result, oneMinusCommissionScaled)
	result.Div(result, scale)

	// result = result * sqrtContribRatio / scaleSqrt
	result.Mul(result, sqrtContribRatio)
	result.Div(result, scaleSqrt)

	// result = result * scale / networkStakingRatioScaled (division)
	result.Mul(result, scale)
	result.Div(result, networkStakingRatioScaled)

	// Result is now in basis points
	return result
}

// sqrtBigInt calculates the integer square root using Newton's method
// For a number scaled by 10^18, the result will be scaled by 10^9
func sqrtBigInt(n *big.Int) *big.Int {
	if n.Cmp(big.NewInt(0)) <= 0 {
		return big.NewInt(0)
	}

	// Initial guess: start with n/2
	x := new(big.Int).Div(n, big.NewInt(2))
	if x.Cmp(big.NewInt(0)) == 0 {
		return big.NewInt(1)
	}

	// Newton's method: x_new = (x + n/x) / 2
	for {
		// Calculate n/x
		nDivX := new(big.Int).Div(n, x)

		// Calculate (x + n/x) / 2
		xNew := new(big.Int).Add(x, nDivX)
		xNew.Div(xNew, big.NewInt(2))

		// Check convergence
		diff := new(big.Int).Sub(x, xNew)
		if diff.Abs(diff).Cmp(big.NewInt(1)) <= 0 {
			return xNew
		}

		x = xNew
	}
}

func (p *Parlia) distributeBasicAndContributionReward(chain consensus.ChainHeaderReader, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) (*big.Int, *big.Int, *big.Int, error) {
	// Use default implementations
	reader := &defaultEthAPIReader{parlia: p}
	writer := &defaultEthAPIWriter{parlia: p, state: state}
	snap, err := p.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward snapshot failed", "blockNumber", header.Number.Uint64()-1, "error", err)
		return nil, nil, nil, err
	}
	return p.distributeBasicAndContributionRewardWithInterfaces(chain, state, header, txs, receipts, receivedTxs, usedGas, mining, tracer, reader, writer, snap)
}

// distributeBasicAndContributionRewardWithInterfaces is the internal implementation that accepts reader and writer interfaces.
// This makes it easier to test by injecting mock implementations.
func (p *Parlia) distributeBasicAndContributionRewardWithInterfaces(chain consensus.ChainHeaderReader, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks,
	reader ethAPIReader, writer ethAPIWriter, snap *Snapshot) (*big.Int, *big.Int, *big.Int, error) {
	blockNr := rpc.BlockNumberOrHashWithHash(header.ParentHash, false)
	cx := chainContext{Chain: chain, parlia: p}
	totalReward := new(big.Int).SetUint64(0)
	additionalBasicReward := new(big.Int).SetUint64(0)
	additionalContributionReward := new(big.Int).SetUint64(0)
	breatheBlockFee := writer.GetBalance(consensus.SystemAddress)
	index := header.Time/params.BreatheBlockInterval - 1
	log.Info("distributeBasicAndContributionReward", "blockNumber", header.Number, "blockTime", header.Time, "intervalIndex", index, "breatheBlockFee", breatheBlockFee)

	annualBlockCountEveryYear := new(big.Int).SetUint64(365 * 24 * 60 * 60 * 1000 / snap.BlockInterval)
	annualBlockCountEveryEpoch := new(big.Int).SetUint64(params.BreatheBlockInterval * 1000 / snap.BlockInterval)
	nominalInterestRate, err := reader.GetNominalInterestRate(blockNr)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward getNominalInterestRate failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
		return nil, nil, nil, err
	}
	inflationRate, err := reader.GetInflationRate(blockNr)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward getInflationRate failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
		return nil, nil, nil, err
	}
	// The first input parameter rate of CalculateRewardByRate is required to be expanded precision, mainly to unify the calculation of contribution rewards
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	nominalInterestRateScaled := new(big.Int).Mul(nominalInterestRate, scale)

	maxContributionRewardRatio, err := reader.GetMaxContributionRewardRatio(blockNr)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward getMaxContributionRewardRatio failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
		return nil, nil, nil, err
	}
	totalIssuedSupply, totalBurnedSupply, err := reader.GetTotalSupply(blockNr)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward getTotalSupply failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
		return nil, nil, nil, err
	}

	validatorsTotalPooled, err := reader.GetValidatorsTotalPooled(blockNr)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward getValidatorsTotalPooled failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
		return nil, nil, nil, err
	}

	log.Debug("distributeBasicAndContributionReward", "annualBlockCountEveryYear", annualBlockCountEveryYear,
		"annualBlockCountEveryEpoch", annualBlockCountEveryEpoch, "nominalInterestRate", nominalInterestRate, "inflationRate", inflationRate,
		"maxContributionRewardRatio", maxContributionRewardRatio, "totalIssuedSupply", totalIssuedSupply, "totalBurnedSupply", totalBurnedSupply,
		"validatorsTotalPooled", validatorsTotalPooled,
	)
	allValidators, err := reader.GetAllValidators(blockNr)
	if err != nil {
		log.Warn("distributeBasicAndContributionReward getAllValidators failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
		return nil, nil, nil, err
	}

	log.Debug("distributeBasicAndContributionReward get validators", "length", len(allValidators))
	writer.SetBalance(consensus.SystemAddress, common.U2560, tracing.BalanceDecreaseBSCDistributeReward)

	// Prepare shared calculation parameters
	totalSupply := new(big.Int).Sub(totalIssuedSupply, totalBurnedSupply)
	maxContributionRewardRate := new(big.Int).Mul(maxContributionRewardRatio, scale)
	upTimeIndex := big.NewInt(int64(index))

	// calculate basic and contribution reward of all validators
	for _, operatorAddr := range allValidators {
		log.Debug("begin calculate basic and contribution reward", "block hash", header.Hash(), "operatorAddr", operatorAddr)

		// Fetch validator-specific data from blockchain state
		validatorData, err := fetchValidatorRewardData(reader, blockNr, operatorAddr, upTimeIndex, header)
		if err != nil {
			log.Warn("distributeBasicAndContributionReward fetchValidatorRewardData failed", "blockNr", blockNr.BlockHash.Hex(), "operatorAddr", operatorAddr, "error", err)
			return nil, nil, nil, err
		}

		// Prepare input for reward calculation
		rewardInput := &ValidatorRewardInput{
			NominalInterestRateScaled:  nominalInterestRateScaled,
			AnnualBlockCountEveryYear:  annualBlockCountEveryYear,
			AnnualBlockCountEveryEpoch: annualBlockCountEveryEpoch,
			TotalPooled:                validatorData.TotalPooled,
			InflationRate:              inflationRate,
			InTurnCounts:               validatorData.InTurnCounts,
			TotalTurnCounts:            validatorData.TotalTurnCounts,
			CommissionRate:             validatorData.CommissionRate,
			TotalDelegated:             validatorData.TotalDelegated,
			ValidatorsTotalPooled:      validatorsTotalPooled,
			TotalSupply:                totalSupply,
			MaxContributionRewardRatio: maxContributionRewardRatio,
		}

		// Calculate rewards using pure function
		rewardResult := calculateValidatorRewards(p, rewardInput, maxContributionRewardRate, scale)

		// Update state and accumulate rewards
		additionalBasicReward.Add(additionalBasicReward, rewardResult.BasicReward)
		additionalContributionReward.Add(additionalContributionReward, rewardResult.ContributionReward)
		totalReward.Add(totalReward, rewardResult.BasicReward)
		totalReward.Add(totalReward, rewardResult.ContributionReward)

		log.Info("calculate basic reward", "blockNumber", header.Number, "operator", operatorAddr,
			"totalDelegated", validatorData.TotalDelegated, "totalPooled", validatorData.TotalPooled,
			"selfDelegated", validatorData.SelfDelegated, "basicReward", rewardResult.BasicReward)
		writer.AddBalance(consensus.SystemAddress, uint256.MustFromBig(rewardResult.BasicReward), tracing.BalanceIncreaseBasicReward)

		if validatorData.ConsensusAddress == header.Coinbase {
			writer.AddBalance(consensus.SystemAddress, breatheBlockFee, tracing.BalanceIncreaseBasicReward)
		}

		log.Info("calculate contribution reward", "blockNumber", header.Number, "operatorAddr", operatorAddr,
			"contributionReward", rewardResult.ContributionReward)
		writer.AddBalance(consensus.SystemAddress, uint256.MustFromBig(rewardResult.ContributionReward), tracing.BalanceIncreaseContributionReward)

		// Distribute fixed block reward (this still requires state access)
		fixedBlockReward, err := writer.DistributeIncoming(validatorData.ConsensusAddress, state, header, cx, txs, receipts, receivedTxs, usedGas, mining, tracer)
		if err != nil {
			log.Warn("distributeBasicAndContributionReward distributeIncoming failed", "blockNr", blockNr.BlockHash.Hex(), "error", err)
			return nil, nil, nil, err
		}
		totalReward.Add(totalReward, fixedBlockReward)
	}
	log.Debug("distributeBasicAndContributionReward", "blockNumber", header.Number, "totalReward", totalReward)
	return totalReward, additionalBasicReward, additionalContributionReward, nil
}

// fetchValidatorRewardDataWithReader fetches all validator-specific data needed for reward calculation from blockchain state.
// This function accepts a reader interface, making it easier to test by injecting mock implementations.
func fetchValidatorRewardData(reader ethAPIReader, blockNr rpc.BlockNumberOrHash, operatorAddr common.Address,
	upTimeIndex *big.Int, header *types.Header) (*validatorRewardData, error) {
	data := &validatorRewardData{}

	// Fetch delegated amounts
	totalDelegated, totalPooled, selfDelegated, err := reader.GetValidatorDelegatedAmount(blockNr, operatorAddr)
	if err != nil {
		return nil, err
	}
	data.TotalDelegated = totalDelegated
	data.TotalPooled = totalPooled
	data.SelfDelegated = selfDelegated

	// Fetch consensus address
	consensusAddress, err := reader.GetValidatorConsensusAddress(blockNr, operatorAddr)
	if err != nil {
		return nil, err
	}
	data.ConsensusAddress = consensusAddress

	// Fetch uptime record
	inTurnCounts, outTurnCounts, err := reader.GetValidatorUptimeRecord(blockNr, consensusAddress, upTimeIndex)
	if err != nil {
		return nil, err
	}
	data.InTurnCounts = inTurnCounts
	data.OutTurnCounts = outTurnCounts
	data.TotalTurnCounts = new(big.Int).Add(inTurnCounts, outTurnCounts)

	// Fetch commission rate
	commissionRate, err := reader.GetValidatorCommissionRate(blockNr, operatorAddr)
	if err != nil {
		return nil, err
	}
	data.CommissionRate = new(big.Int).SetUint64(commissionRate.Rate)

	log.Debug("fetchValidatorRewardData", "blockNumber", header.Number, "operator", operatorAddr,
		"consensusAddress", consensusAddress, "inTurnCounts", inTurnCounts, "outTurnCounts", outTurnCounts,
		"commissionRate", commissionRate)

	return data, nil
}

// calculateValidatorRewards is a pure function that calculates validator rewards based on input data.
// This function has no dependencies on blockchain state, making it easy to test.
func calculateValidatorRewards(p *Parlia, input *ValidatorRewardInput, maxContributionRewardRate *big.Int, scale *big.Int) *ValidatorRewardResult {
	result := &ValidatorRewardResult{}

	// Calculate basic reward: (1 + nominalInterestRate/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1
	result.BasicReward = p.CalculateRewardByRate(
		input.NominalInterestRateScaled,
		input.AnnualBlockCountEveryYear,
		input.AnnualBlockCountEveryEpoch,
		input.TotalPooled,
	)

	// Calculate contribution reward rate
	contributionRewardRate := CalculateContributionRewardRate(
		input.InflationRate,
		input.InTurnCounts,
		input.TotalTurnCounts,
		input.CommissionRate,
		input.TotalDelegated,
		input.TotalPooled,
		input.ValidatorsTotalPooled,
		input.TotalSupply,
	)

	// Cap contribution reward rate at maximum
	if contributionRewardRate.Cmp(maxContributionRewardRate) > 0 {
		contributionRewardRate = new(big.Int).Set(maxContributionRewardRate)
	}

	// Calculate contribution reward with compound interest:
	// totalPooled * ((1 + contributionRewardRate/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1)
	result.ContributionReward = p.CalculateRewardByRate(
		contributionRewardRate,
		input.AnnualBlockCountEveryYear,
		input.AnnualBlockCountEveryEpoch,
		input.TotalPooled,
	)

	return result
}

// slash spoiled validators
func (p *Parlia) updateValidatorUptimeRecord(spoiledVal common.Address, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// method
	method := "updateValidatorUptimeRecord"
	index := header.Time / params.BreatheBlockInterval
	// get packed data
	data, err := p.validatorSetABI.Pack(method,
		new(big.Int).SetUint64(index), header.Coinbase, spoiledVal,
	)
	if err != nil {
		log.Error("Unable to pack tx for updateValidatorUptimeRecord", "error", err)
		return err
	}
	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

func (p *Parlia) _updateCurrentTotalSupply(state *vm.StateDB, header *types.Header, additionalAmount *big.Int, burnedAmount *big.Int, additionalBasicRewardAmount *big.Int, additionalContributionRewardAmount *big.Int, chain core.ChainContext, txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	method := "updateCurrentTotalSupply"
	data, err := p.validatorSetABI.Pack(method, additionalAmount, burnedAmount, additionalBasicRewardAmount, additionalContributionRewardAmount)
	if err != nil {
		log.Error("Unable to pack tx for updateCurrentTotalIssuedSupply", "error", err)
		return err
	}

	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, *state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

func (p *Parlia) updateCurrentTotalSupply(additionalIssuanceAmount *big.Int, additionalBasicRewardAmount *big.Int, additionalContributionRewardAmount *big.Int,
	chain consensus.ChainHeaderReader, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	cx := chainContext{Chain: chain, parlia: p}
	blockNr := rpc.BlockNumberOrHashWithHash(header.ParentHash, false)
	// BurnedAddressList := []common.Address{
	// 	common.HexToAddress("0x0000000000000000000000000000000000000000"),
	// 	common.HexToAddress("0x000000000000000000000000000000000000DEAD"),
	// }
	burnedAddressList, err := p.getBurnedAddressList(blockNr)
	if err != nil {
		log.Error("Failed to get burned address list", "error", err)
		return err
	}
	log.Info("updateCurrentTotalSupply", "blockNumber", header.Number, "burnedAddressList", burnedAddressList)
	// Get balance at specific block number using ethAPI
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	burnedAmount := new(big.Int).SetUint64(0)

	for _, address := range burnedAddressList {
		balance, err := p.ethAPI.GetBalance(ctx, address, blockNr)
		if err != nil {
			log.Error("Failed to get balance for burned address", "address", address, "blockNumber", header.Number, "error", err)
			// Fallback to current state if API call fails
			burnedAmount.Add(burnedAmount, state.GetBalance(address).ToBig())
		} else {
			burnedAmount.Add(burnedAmount, balance.ToInt())
		}
	}

	log.Info("updateCurrentTotalSupply", "blockNumber", header.Number, "additionalIssuanceAmount", additionalIssuanceAmount, "burnedAmount", burnedAmount, "additionalBasicRewardAmount", additionalBasicRewardAmount, "additionalContributionRewardAmount", additionalContributionRewardAmount)
	return p._updateCurrentTotalSupply(&state, header, additionalIssuanceAmount, burnedAmount, additionalBasicRewardAmount, additionalContributionRewardAmount, cx, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

func (p *Parlia) getInflationRecord(year int, blockNr rpc.BlockNumberOrHash) (*big.Int, *big.Int, *big.Int, *big.Int, *big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "inflationRecord"
	data, err := p.validatorSetABI.Pack(method, new(big.Int).SetInt64(int64(year)))
	if err != nil {
		log.Error("Unable to pack tx for getInflationRecord", "error", err)
		return nil, nil, nil, nil, nil, err
	}
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}

	return unpacked[0].(*big.Int), unpacked[1].(*big.Int), unpacked[2].(*big.Int), unpacked[3].(*big.Int), unpacked[4].(*big.Int), nil
}

func (p *Parlia) getInflationRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "inflationRate"
	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getInflationRate", "error", err)
		return nil, err
	}
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, err
	}

	return unpacked[0].(*big.Int), nil
}

func (p *Parlia) updateInflationRecord(year int, totalSupply *big.Int, additionalIssuanceAmount *big.Int, additionalBasicIssuanceAmount *big.Int, additionalContributionIssuanceAmount *big.Int, newInflationRate *big.Int, chain core.ChainContext, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	method := "updateInflationRecord"
	data, err := p.validatorSetABI.Pack(method, new(big.Int).SetInt64(int64(year)), additionalIssuanceAmount, additionalBasicIssuanceAmount, additionalContributionIssuanceAmount, totalSupply, newInflationRate)
	if err != nil {
		log.Error("Unable to pack tx for updateInflationInfo", "error", err)
		return err
	}

	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

// calculateNewYearInflation returns the additional issuance amount and the new inflation rate (basis points)
// based on current total supply and last year's total supply.
func calculateNewYearInflation(currentTotalSupply, lastYearTotalSupply *big.Int) (*big.Int, *big.Int) {
	additionalIssuanceAmount := new(big.Int).Sub(currentTotalSupply, lastYearTotalSupply)
	if additionalIssuanceAmount.Cmp(big.NewInt(0)) <= 0 {
		return big.NewInt(0), big.NewInt(0)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	additionalIssuanceAmountScaled := new(big.Int).Mul(additionalIssuanceAmount, scale)
	additionalIssuanceAmountScaled.Mul(additionalIssuanceAmountScaled, new(big.Int).SetUint64(10000))
	newInflationRate := new(big.Int).Div(additionalIssuanceAmountScaled, currentTotalSupply)
	newInflationRate = newInflationRate.Div(newInflationRate, scale)
	log.Debug("calculateNewYearInflation", "currentTotalSupply", currentTotalSupply, "lastYearTotalSupply", lastYearTotalSupply, "additionalIssuanceAmount", additionalIssuanceAmount, "newInflationRate", newInflationRate)
	// max inflation rate is 500 basis points
	if newInflationRate.Cmp(big.NewInt(MaxInflationRate)) > 0 {
		newInflationRate = big.NewInt(MaxInflationRate)
	}
	return additionalIssuanceAmount, newInflationRate
}

func (p *Parlia) updateInflationRecordForNewYear(parentYear int, currentYear int, cx core.ChainContext, state vm.StateDB,
	header *types.Header, txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	blockNr := rpc.BlockNumberOrHashWithHash(header.ParentHash, false)
	log.Info("First block of new year detected", "blockNumber", header.Number, "blockTime", header.Time, "parentYear", parentYear, "currentYear", currentYear)
	currentTotalSupply, _, err := p.getTotalSupply(blockNr)
	if err != nil {
		return err
	}
	totalIssuanceAmountOfBasicReward, totalIssuanceAmountOfContributionReward, err := p.getTotalIssuanceAmountOfReward(blockNr)
	if err != nil {
		return err
	}
	lastYearTotalSupply, _, lastYearAdditionalBasicRewardAmount, lastYearAdditionalContributionRewardAmount, _, err := p.getInflationRecord(parentYear, blockNr)
	if err != nil {
		return err
	}
	additionalIssuanceAmount, newInflationRate := calculateNewYearInflation(currentTotalSupply, lastYearTotalSupply)
	additionalBasicIssuanceAmount := new(big.Int).Sub(totalIssuanceAmountOfBasicReward, lastYearAdditionalBasicRewardAmount)
	additionalContributionIssuanceAmount := new(big.Int).Sub(totalIssuanceAmountOfContributionReward, lastYearAdditionalContributionRewardAmount)
	log.Info("updateInflationRecordForNewYear", "blockNumber", header.Number, "blockTime", header.Time, "additionalIssuanceAmount", additionalIssuanceAmount, "inflationRate", newInflationRate, "additionalBasicIssuanceAmount", additionalBasicIssuanceAmount, "additionalContributionIssuanceAmount", additionalContributionIssuanceAmount)
	// Update total supply information for the new year
	err = p.updateInflationRecord(currentYear, currentTotalSupply, additionalIssuanceAmount, additionalBasicIssuanceAmount, additionalContributionIssuanceAmount, newInflationRate, cx, state, header, txs, receipts, receivedTxs, usedGas, mining, tracer)
	if err != nil {
		return err
	}
	return nil
}

func (p *Parlia) getBurnedAddressList(blockNr rpc.BlockNumberOrHash) ([]common.Address, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getBurnedAddressList"
	data, err := p.validatorSetABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getBurnedAddressList", "error", err)
		return nil, err
	}
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))
	msgData := (hexutil.Bytes)(data)

	result, err := p.ethAPI.Call(ctx, ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		return nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, err
	}
	return unpacked[0].([]common.Address), nil
}

// defaultEthAPIReader is the default implementation of ethAPIReader interface.
// It delegates all calls to the underlying Parlia instance methods.
type defaultEthAPIReader struct {
	parlia *Parlia
}

func (r *defaultEthAPIReader) GetNominalInterestRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	return r.parlia.getNominalInterestRate(blockNr)
}

func (r *defaultEthAPIReader) GetInflationRate(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	return r.parlia.getInflationRate(blockNr)
}

func (r *defaultEthAPIReader) GetMaxContributionRewardRatio(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	return r.parlia.getMaxContributionRewardRatio(blockNr)
}

func (r *defaultEthAPIReader) GetTotalSupply(blockNr rpc.BlockNumberOrHash) (*big.Int, *big.Int, error) {
	return r.parlia.getTotalSupply(blockNr)
}

func (r *defaultEthAPIReader) GetValidatorsTotalPooled(blockNr rpc.BlockNumberOrHash) (*big.Int, error) {
	return r.parlia.getValidatorsTotalPooled(blockNr)
}

func (r *defaultEthAPIReader) GetAllValidators(blockNr rpc.BlockNumberOrHash) ([]common.Address, error) {
	return r.parlia.getAllValidators(blockNr)
}

func (r *defaultEthAPIReader) GetValidatorDelegatedAmount(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*big.Int, *big.Int, *big.Int, error) {
	return r.parlia.getValidatorDelegatedAmount(blockNr, operatorAddr)
}

func (r *defaultEthAPIReader) GetValidatorConsensusAddress(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (common.Address, error) {
	return r.parlia.getValidatorConsensusAddress(blockNr, operatorAddr)
}

func (r *defaultEthAPIReader) GetValidatorUptimeRecord(blockNr rpc.BlockNumberOrHash, val common.Address, index *big.Int) (*big.Int, *big.Int, error) {
	return r.parlia.getValidatorUptimeRecord(blockNr, val, index)
}

func (r *defaultEthAPIReader) GetValidatorCommissionRate(blockNr rpc.BlockNumberOrHash, val common.Address) (CommissionItem, error) {
	return r.parlia.getValidatorCommissionRate(blockNr, val)
}

// defaultEthAPIWriter is the default implementation of ethAPIWriter interface.
// It delegates all calls to the underlying state database and Parlia instance methods.
type defaultEthAPIWriter struct {
	parlia *Parlia
	state  vm.StateDB
}

func (w *defaultEthAPIWriter) SetBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	w.state.SetBalance(addr, amount, reason)
}

func (w *defaultEthAPIWriter) AddBalance(addr common.Address, amount *uint256.Int, reason tracing.BalanceChangeReason) {
	w.state.AddBalance(addr, amount, reason)
}

func (w *defaultEthAPIWriter) GetBalance(addr common.Address) *uint256.Int {
	return w.state.GetBalance(addr)
}

func (w *defaultEthAPIWriter) DistributeIncoming(val common.Address, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) (*big.Int, error) {
	return w.parlia.distributeIncoming(val, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}
