package hotstuff

import (
	"context"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/systemcontracts"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
)

// GetValidators retrieves validators from the StakeHubContract
// It returns operator addresses, credit addresses, and total length of validators
func (h *Hotstuff) GetValidators(offset, limit *big.Int) ([]common.Address, []common.Address, *big.Int, error) {
	log.Debug("Getting validators from latest block", "offset", offset, "limit", limit)

	// Create the call data for getValidators
	data, err := h.stakeHubABI.Pack("getValidators", offset, limit)
	if err != nil {
		log.Error("Failed to pack stakehub getValidators", "error", err)
		return nil, nil, nil, fmt.Errorf("failed to pack getValidators: %v", err)
	}

	// Make the call
	blockNr := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	msgData := (hexutil.Bytes)(data)
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	log.Debug("Calling getValidators from latest block", "to", toAddress)
	result, err := h.ethAPI.Call(context.Background(), ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		log.Error("Failed to call stakehub getValidators", "error", err)
		return nil, nil, nil, fmt.Errorf("failed to call stakehub getValidators: %v", err)
	}

	// Unpack the result
	var operatorAddrs []common.Address
	var creditAddrs []common.Address
	var totalLength *big.Int
	if err := h.stakeHubABI.UnpackIntoInterface(&[]interface{}{&operatorAddrs, &creditAddrs, &totalLength}, "getValidators", result); err != nil {
		log.Error("Failed to unpack stakehub getValidators result", "error", err)
		return nil, nil, nil, fmt.Errorf("failed to unpack getValidators result: %v", err)
	}

	log.Debug("Successfully retrieved stakehub validators", "operators", len(operatorAddrs), "credits", len(creditAddrs), "total", totalLength)
	return operatorAddrs, creditAddrs, totalLength, nil
}

// getNodeIDsForValidators retrieves node IDs for the given validators
// It returns a map of consensus addresses to their node IDs
func (h *Hotstuff) getNodeIDsForValidators(validatorsToQuery []common.Address) (map[common.Address][]enode.ID, error) {
	log.Debug("Listing node IDs for validators from latest block", "validators", len(validatorsToQuery))

	// Create the call data for getNodeIDs
	data, err := h.stakeHubABI.Pack("getNodeIDs", validatorsToQuery)
	if err != nil {
		log.Error("Failed to pack getNodeIDs", "error", err)
		return nil, fmt.Errorf("failed to pack getNodeIDs: %v", err)
	}

	// Make the call
	blockNr := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	msgData := (hexutil.Bytes)(data)
	toAddress := common.HexToAddress(systemcontracts.StakeHubContract)
	gas := (hexutil.Uint64)(uint64(math.MaxUint64 / 2))

	log.Debug("Calling getNodeIDs from latest block", "to", toAddress)
	result, err := h.ethAPI.Call(context.Background(), ethapi.TransactionArgs{
		Gas:  &gas,
		To:   &toAddress,
		Data: &msgData,
	}, &blockNr, nil, nil)
	if err != nil {
		log.Error("Failed to call getNodeIDs", "error", err)
		return nil, fmt.Errorf("failed to call getNodeIDs: %v", err)
	}

	// Unpack the result
	var consensusAddresses []common.Address
	var nodeIDsList [][]enode.ID
	if err := h.stakeHubABI.UnpackIntoInterface(&[]interface{}{&consensusAddresses, &nodeIDsList}, "getNodeIDs", result); err != nil {
		log.Error("Failed to unpack getNodeIDs result", "error", err)
		return nil, fmt.Errorf("failed to unpack getNodeIDs result: %v", err)
	}

	// Create a map of addresses to node IDs
	addressToNodeIDs := make(map[common.Address][]enode.ID)
	for i, addr := range consensusAddresses {
		if i < len(nodeIDsList) {
			addressToNodeIDs[addr] = nodeIDsList[i]
			log.Debug("getNodeIDsForValidators: mapping", "consensusAddr", addr.Hex(), "nodeIDsCount", len(nodeIDsList[i]))
		}
	}

	log.Debug("Successfully retrieved node IDs", "addresses", len(addressToNodeIDs))
	return addressToNodeIDs, nil
}

// GetNodeIDs returns a flattened array of all node IDs for current validators
func (h *Hotstuff) GetNodeIDs() ([]enode.ID, error) {
	// Call GetValidators with latest block number
	operatorAddrs, _, _, err := h.GetValidators(big.NewInt(0), big.NewInt(1000))
	if err != nil {
		log.Error("Failed to get validators", "error", err)
		return nil, fmt.Errorf("failed to get validators: %v", err)
	}
	log.Debug("Retrieved validators", "count", len(operatorAddrs))

	// Get node IDs for validators
	nodeIDs, err := h.getNodeIDsForValidators(operatorAddrs)
	if err != nil {
		log.Error("Failed to get node IDs", "error", err)
		return nil, fmt.Errorf("failed to get node IDs: %v", err)
	}
	log.Debug("Retrieved node IDs map", "addresses", len(nodeIDs))

	// Flatten the array of arrays into a single array
	flatNodeIDs := make([]enode.ID, 0)
	for addr, nodeIDArray := range nodeIDs {
		flatNodeIDs = append(flatNodeIDs, nodeIDArray...)
		log.Debug("Processing node IDs", "address", addr, "count", len(nodeIDArray))
	}

	log.Debug("Successfully flattened node IDs", "total", len(flatNodeIDs))
	return flatNodeIDs, nil
}

// AddNodeIDs creates a signed transaction to add node IDs to the StakeHub contract
func (h *Hotstuff) AddNodeIDs(nodeIDs []enode.ID, nonce uint64) (*types.Transaction, error) {
	log.Debug("Adding node IDs", "count", len(nodeIDs), "nonce", nonce)

	val, _, signTxFn := h.signerCredentials()

	if signTxFn == nil {
		log.Error("Signing function not set")
		return nil, fmt.Errorf("signing function not set, call Authorize first")
	}

	// Create the call data for addNodeIDs
	data, err := h.stakeHubABI.Pack("addNodeIDs", nodeIDs)
	if err != nil {
		log.Error("Failed to pack addNodeIDs", "error", err)
		return nil, fmt.Errorf("failed to pack addNodeIDs: %v", err)
	}

	to := common.HexToAddress(systemcontracts.StakeHubContract)
	hexData := hexutil.Bytes(data)
	hexNonce := hexutil.Uint64(nonce)
	gas, err := h.ethAPI.EstimateGas(context.Background(), ethapi.TransactionArgs{
		From:  &val,
		To:    &to,
		Nonce: &hexNonce,
		Data:  &hexData,
	}, nil, nil, nil)
	if err != nil {
		log.Error("Failed to estimate gas", "error", err)
		return nil, fmt.Errorf("failed to estimate gas: %v", err)
	}

	// Create the transaction
	tx := types.NewTransaction(
		nonce,
		common.HexToAddress(systemcontracts.StakeHubContract),
		common.Big0,
		uint64(gas),
		big.NewInt(1000000000),
		data,
	)

	// Sign the transaction with the node's private key
	log.Debug("Signing transaction", "validator", val)
	signedTx, err := signTxFn(accounts.Account{Address: val}, tx, h.chainConfig.ChainID)
	if err != nil {
		log.Error("Failed to sign transaction", "error", err)
		return nil, fmt.Errorf("failed to sign transaction: %v", err)
	}

	log.Debug("Successfully created signed transaction", "hash", signedTx.Hash())
	return signedTx, nil
}

// GetNodeIDsMap returns a map of consensus addresses to their node IDs for all current validators
func (h *Hotstuff) GetNodeIDsMap() (map[common.Address][]enode.ID, error) {
	// Call GetValidators with latest block number
	operatorAddrs, _, _, err := h.GetValidators(big.NewInt(0), big.NewInt(1000))
	if err != nil {
		log.Error("Failed to get validators", "error", err)
		return nil, fmt.Errorf("failed to get validators: %v", err)
	}
	log.Debug("GetNodeIDsMap: retrieved validators (operators)", "count", len(operatorAddrs))
	for i, addr := range operatorAddrs {
		log.Debug("GetNodeIDsMap: operator address", "index", i, "addr", addr.Hex())
	}

	// Get node IDs for validators
	// Note: getNodeIDsForValidators accepts operator addresses and returns a map keyed by consensus addresses
	nodeIDsMap, err := h.getNodeIDsForValidators(operatorAddrs)
	if err != nil {
		log.Error("Failed to get node IDs", "error", err)
		return nil, fmt.Errorf("failed to get node IDs: %v", err)
	}
	log.Debug("GetNodeIDsMap: retrieved node IDs map (consensus addresses)", "addresses", len(nodeIDsMap))

	return nodeIDsMap, nil
}

// RemoveNodeIDs creates a signed transaction to remove node IDs from the StakeHub contract
func (h *Hotstuff) RemoveNodeIDs(nodeIDs []enode.ID, nonce uint64) (*types.Transaction, error) {
	log.Debug("Removing node IDs", "count", len(nodeIDs), "nonce", nonce)

	val, _, signTxFn := h.signerCredentials()

	if signTxFn == nil {
		log.Error("Signing function not set")
		return nil, fmt.Errorf("signing function not set, call Authorize first")
	}

	// Create the call data for removeNodeIDs
	data, err := h.stakeHubABI.Pack("removeNodeIDs", nodeIDs)
	if err != nil {
		log.Error("Failed to pack removeNodeIDs", "error", err)
		return nil, fmt.Errorf("failed to pack removeNodeIDs: %v", err)
	}

	to := common.HexToAddress(systemcontracts.StakeHubContract)
	hexData := hexutil.Bytes(data)
	hexNonce := hexutil.Uint64(nonce)
	gas, err := h.ethAPI.EstimateGas(context.Background(), ethapi.TransactionArgs{
		From:  &val,
		To:    &to,
		Nonce: &hexNonce,
		Data:  &hexData,
	}, nil, nil, nil)
	if err != nil {
		log.Error("Failed to estimate gas", "error", err)
		return nil, fmt.Errorf("failed to estimate gas: %v", err)
	}

	// Create the transaction
	tx := types.NewTransaction(
		nonce,
		common.HexToAddress(systemcontracts.StakeHubContract),
		common.Big0,
		uint64(gas),
		big.NewInt(1000000000),
		data,
	)

	// Sign the transaction with the node's private key
	log.Debug("Signing transaction", "validator", val)
	signedTx, err := signTxFn(accounts.Account{Address: val}, tx, h.chainConfig.ChainID)
	if err != nil {
		log.Error("Failed to sign transaction", "error", err)
		return nil, fmt.Errorf("failed to sign transaction: %v", err)
	}

	log.Debug("Successfully created signed transaction", "hash", signedTx.Hash())
	return signedTx, nil
}
