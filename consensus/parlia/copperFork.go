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

func (p *Parlia) getTotalPooled(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorTotalPooled"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getValidatorTotalPooled", "error", err)
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

func (p *Parlia) getSelfDelegated(blockNr rpc.BlockNumberOrHash, credit common.Address) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorSelfDelegated"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method)
	if err != nil {
		log.Error("Unable to pack tx for getValidatorSelfDelegated", "error", err)
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

func (p *Parlia) getValidatorDelegatedAmount(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*big.Int, *big.Int, *big.Int, error) {
	totalPooled, err := p.getTotalPooled(blockNr, operatorAddr)
	if err != nil {
		log.Error("Unable to get total pooled", "error", err)
		return nil, nil, nil, err
	}
	selfDelegated, err := p.getSelfDelegated(blockNr, operatorAddr)
	if err != nil {
		log.Error("Unable to get self delegated", "error", err)
		return nil, nil, nil, err
	}
	totalDelegated := new(big.Int).Sub(totalPooled, selfDelegated)
	return totalDelegated, totalPooled, selfDelegated, nil
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

	data, err := p.validatorSetABI.Pack(method)
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

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, err
	}

	return unpacked[0].(*big.Int), nil
}

func (p *Parlia) getValidatorConsensusAddress(blockNr rpc.BlockNumberOrHash, operatorAddr common.Address) (*common.Address, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorConsensusAddress"
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.stakeHubABI.Pack(method, operatorAddr)
	if err != nil {
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

	return unpacked[0].(*common.Address), nil
}
func (p *Parlia) getValidatorCommissionRate(blockNr rpc.BlockNumberOrHash, val common.Address) (*big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "getValidatorCommission"
	toAddress := common.HexToAddress(systemcontracts.ValidatorContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	data, err := p.validatorSetABI.Pack(method, val)
	if err != nil {
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

func (p *Parlia) distributeBasicAndContributionReward(chain consensus.ChainHeaderReader, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, tracer *tracing.Hooks) (*big.Int, error) {
	blockNr := rpc.BlockNumberOrHashWithHash(header.ParentHash, false)
	cx := chainContext{Chain: chain, parlia: p}
	totalReward := new(big.Int).SetUint64(0)
	breatheBlockFee := state.GetBalance(consensus.SystemAddress)
	index := header.Time/params.BreatheBlockInterval - 1
	snap, err := p.snapshot(chain, header.Number.Uint64()-1, header.ParentHash, nil)
	if err != nil {
		return nil, err
	}
	annualBlockCountEveryYear := new(big.Int).SetUint64(365 * 24 * 60 * 60 * 1000 / snap.BlockInterval)
	annualBlockCountEveryEpoch := new(big.Int).SetUint64(params.BreatheBlockInterval * 1000 / snap.BlockInterval)
	nominalInterestRate, err := p.getNominalInterestRate(blockNr)
	if err != nil {
		return nil, err
	}

	totalIssuedSupply, totalBurnedSupply, err := p.getTotalSupply(blockNr)
	if err != nil {
		return nil, err
	}

	validatorsTotalPooled, err := p.getValidatorsTotalPooled(blockNr)
	if err != nil {
		return nil, err
	}

	allValidators, err := p.getAllValidators(blockNr)
	if err != nil {
		return nil, err
	}

	state.SetBalance(consensus.SystemAddress, common.U2560, tracing.BalanceDecreaseBSCDistributeReward)
	// calculate basic and contribution reward of all validators
	for _, operatorAddr := range allValidators {
		totalDelegated, totalPooled, selfDelegated, err := p.getValidatorDelegatedAmount(blockNr, operatorAddr)
		if err != nil {
			return nil, err
		}

		log.Debug("getValidatorDelegatedAmount", "block hash", header.Hash(), "totalDelegated", totalDelegated, "totalPooled", totalPooled, "selfDelegated", selfDelegated)
		log.Debug("annualBlockCountEveryYear", "block hash", header.Hash(), "annualBlockCountEveryYear", annualBlockCountEveryYear)
		log.Debug("annualBlockCountEveryEpoch", "block hash", header.Hash(), "annualBlockCountEveryEpoch", annualBlockCountEveryEpoch)

		// Calculate (1 + nominalInterestRate/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1
		basicReward := p.CalculateRewardByRate(nominalInterestRate, annualBlockCountEveryYear, annualBlockCountEveryEpoch, totalPooled)

		log.Info("calculate basic reward", "block hash", header.Hash(), "address", p.val, "totalPooled", totalPooled, "basicReward", basicReward)
		state.AddBalance(consensus.SystemAddress, uint256.MustFromBig(basicReward), tracing.BalanceIncreaseBasicReward)
		totalReward.Add(totalReward, basicReward)
		// calculate validator uptime rate
		consensusAddress, err := p.getValidatorConsensusAddress(blockNr, operatorAddr)
		if err != nil {
			return nil, err
		}
		if *consensusAddress == header.Coinbase {
			state.AddBalance(consensus.SystemAddress, breatheBlockFee, tracing.BalanceIncreaseBasicReward)
		}

		upTimeIndex := big.NewInt(int64(index))
		inTurnCounts, outTurnCounts, err := p.getValidatorUptimeRecord(blockNr, *consensusAddress, upTimeIndex)
		if err != nil {
			return nil, err
		}

		// Calculate uptime rate: inTurnCounts / (inTurnCounts + outTurnCounts)
		totalTurnCounts := new(big.Int).Add(inTurnCounts, outTurnCounts)
		uptimeRate := big.NewInt(0)
		if totalTurnCounts.Cmp(big.NewInt(0)) > 0 {
			uptimeRate = new(big.Int).Mul(inTurnCounts, big.NewInt(10000))
			uptimeRate.Div(uptimeRate, totalTurnCounts)
		}
		log.Debug("uptimeRate", "block hash", header.Hash(), "uptimeRate", uptimeRate)

		// Calculate contribution staking ratio: totalDelegated / totalPooled
		contributionStakingRatio := new(big.Int).Mul(totalDelegated, big.NewInt(10000))
		contributionStakingRatio.Div(contributionStakingRatio, totalPooled)
		log.Debug("contributionStakingRatio", "block hash", header.Hash(), "contributionStakingRatio", contributionStakingRatio)
		// Calculate total network-wide staking ratio: validatorsTotalPooled / (totalIssuedSupply - totalBurnedSupply)
		totalSupply := new(big.Int).Sub(totalIssuedSupply, totalBurnedSupply)
		totalNetworkStakingRatio := new(big.Int).Mul(validatorsTotalPooled, big.NewInt(10000))
		totalNetworkStakingRatio.Div(totalNetworkStakingRatio, totalSupply)
		log.Debug("totalNetworkStakingRatio", "block hash", header.Hash(), "totalNetworkStakingRatio", totalNetworkStakingRatio)

		inflationRate, err := p.getInflationRate(blockNr)
		if err != nil {
			return nil, err
		}
		commissionRateBig, err := p.getValidatorCommissionRate(blockNr, operatorAddr)
		if err != nil {
			return nil, err
		}
		log.Debug("commissionRate", "block hash", header.Hash(), "commissionRateBig", commissionRateBig)

		// Calculate contribution reward rate
		// contributionRewardRate = inflationRate * uptimeRate * (1 - commissionRate) * totalNetworkStakingRatio * sqrt(contributionStakingRatio)
		contributionRewardRate := new(big.Int).Mul(inflationRate, uptimeRate)
		contributionRewardRate.Mul(contributionRewardRate, new(big.Int).Sub(big.NewInt(10000), commissionRateBig))
		contributionRewardRate.Mul(contributionRewardRate, totalNetworkStakingRatio)
		contributionRewardRate.Mul(contributionRewardRate, contributionStakingRatio)

		log.Debug("contributionRewardRate", "block hash", header.Hash(), "contributionRewardRate", contributionRewardRate)

		// Calculate contribution reward with compound interest: totalPooled * ((1 + contributionRewardRate/annualBlockCountEveryYear)^annualBlockCountEveryEpoch - 1)
		contributionReward := p.CalculateRewardByRate(contributionRewardRate, annualBlockCountEveryYear, annualBlockCountEveryEpoch, totalPooled)
		totalReward.Add(totalReward, contributionReward)
		state.AddBalance(*consensusAddress, uint256.MustFromBig(contributionReward), tracing.BalanceIncreaseContributionReward)
		log.Info("calculate contribution reward", "block hash", header.Hash(), "operatorAddr", operatorAddr, "consensusAddr", *consensusAddress,
			"inTurnCounts", inTurnCounts, "outTurnCounts", outTurnCounts, "uptimeRate", uptimeRate,
			"contributionStakingRatio", contributionStakingRatio, "totalNetworkStakingRatio", totalNetworkStakingRatio,
			"commissionRate", commissionRateBig, "contributionRewardRate", contributionRewardRate, "contributionReward", contributionReward)

		// todo: If there are a large number of validators, consider adding a batch deposit interface to the contract.
		fixedBlockReward, err := p.distributeIncoming(*consensusAddress, state, header, cx, txs, receipts, receivedTxs, usedGas, true, tracer)
		if err != nil {
			return nil, err
		}
		totalReward.Add(totalReward, fixedBlockReward)
	}
	return totalReward, nil
}

// slash spoiled validators
func (p *Parlia) updateValidatorUptimeRecords(spoiledVal common.Address, state vm.StateDB, header *types.Header, chain core.ChainContext,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	// method
	method := "updateValidatorUptimeRecords"
	index := header.Time / params.BreatheBlockInterval
	// get packed data
	data, err := p.validatorSetABI.Pack(method,
		index, header.Coinbase, spoiledVal,
	)
	if err != nil {
		log.Error("Unable to pack tx for updateValidatorUptimeRecords", "error", err)
		return err
	}
	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.SlashContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

func (p *Parlia) _updateCurrentTotalSupply(state *vm.StateDB, header *types.Header, additionalAmount *big.Int, burnedAmount *big.Int, chain core.ChainContext, txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	method := "updateCurrentTotalSupply"
	data, err := p.validatorSetABI.Pack(method, additionalAmount, burnedAmount)
	if err != nil {
		log.Error("Unable to pack tx for updateCurrentTotalIssuedSupply", "error", err)
		return err
	}

	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, *state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

func (p *Parlia) updateCurrentTotalSupply(additionalIssuanceAmount *big.Int, chain consensus.ChainHeaderReader, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	cx := chainContext{Chain: chain, parlia: p}
	blockNr := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(header.Number.Uint64()))
	// BurnedAddressList := []common.Address{
	// 	common.HexToAddress("0x0000000000000000000000000000000000000000"),
	// 	common.HexToAddress("0x000000000000000000000000000000000000DEAD"),
	// }
	burnedAddressList, err := p.getBurnedAddressList(blockNr)
	if err != nil {
		return err
	}
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

	return p._updateCurrentTotalSupply(&state, header, additionalIssuanceAmount, burnedAmount, cx, txs, receipts, receivedTxs, &header.GasUsed, true, tracer)
}

func (p *Parlia) getInflationRecord(year int, blockNr rpc.BlockNumberOrHash, validatorAddr common.Address) (*big.Int, *big.Int, *big.Int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "inflationRecord"
	data, err := p.validatorSetABI.Pack(method, year)
	if err != nil {
		log.Error("Unable to pack tx for getInflationRecord", "error", err)
		return nil, nil, nil, err
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
		return nil, nil, nil, err
	}

	unpacked, err := p.validatorSetABI.Unpack(method, result)
	if err != nil {
		return nil, nil, nil, err
	}

	return unpacked[0].(*big.Int), unpacked[1].(*big.Int), unpacked[2].(*big.Int), nil
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

func (p *Parlia) updateInflationRecord(year int, totalSupply *big.Int, additionalIssuanceAmount *big.Int, inflationRateUint uint64, chain core.ChainContext, state vm.StateDB, header *types.Header,
	txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	method := "updateInflationRecord"
	data, err := p.validatorSetABI.Pack(method, year, totalSupply, additionalIssuanceAmount, inflationRateUint)
	if err != nil {
		log.Error("Unable to pack tx for updateInflationInfo", "error", err)
		return err
	}

	// get system message
	msg := p.getSystemMessage(header.Coinbase, common.HexToAddress(systemcontracts.ValidatorContract), data, common.Big0)
	// apply message
	return p.applyTransaction(msg, state, header, chain, txs, receipts, receivedTxs, usedGas, mining, tracer)
}

func (p *Parlia) updateInflationRecordForNewYear(parentYear int, currentYear int, cx core.ChainContext, state vm.StateDB,
	header *types.Header, txs *[]*types.Transaction, receipts *[]*types.Receipt, receivedTxs *[]*types.Transaction, usedGas *uint64, mining bool, tracer *tracing.Hooks) error {
	blockNr := rpc.BlockNumberOrHashWithNumber(rpc.BlockNumber(header.Number.Uint64()))
	log.Info("First block of new year detected", "blockNumber", header.Number, "blockTime", header.Time, "parentYear", parentYear, "currentYear", currentYear)
	currentTotalSupply, _, err := p.getTotalSupply(blockNr)
	if err != nil {
		return err
	}
	lastYearTotalSupply, _, _, err := p.getInflationRecord(parentYear, blockNr, p.val)
	if err != nil {
		return err
	}
	additionalIssuanceAmount := new(big.Int).Sub(currentTotalSupply, lastYearTotalSupply)
	newInflationRate := new(big.Float).Quo(new(big.Float).SetInt(additionalIssuanceAmount), new(big.Float).SetInt(lastYearTotalSupply))
	inflationRateUint, _ := newInflationRate.Mul(newInflationRate, big.NewFloat(10000)).Uint64()
	log.Info("Inflation rate", "blockNumber", header.Number, "blockTime", header.Time, "inflationRate", inflationRateUint)
	// Update total supply information for the new year
	err = p.updateInflationRecord(currentYear, currentTotalSupply, additionalIssuanceAmount, inflationRateUint, cx, state, header, txs, receipts, receivedTxs, &header.GasUsed, true, tracer)
	if err != nil {
		return err
	}
	return nil
}

func (p *Parlia) getBurnedAddressList(blockNr rpc.BlockNumberOrHash) ([]common.Address, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	method := "burnedAddressList"
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
