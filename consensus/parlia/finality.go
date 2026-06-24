package parlia

import (
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/willf/bitset"
)

func fastFinalityQuorum(validators int) int {
	return cmath.CeilDiv(validators*2, 3)
}

// optimisticFinalityQuorum is the one-round finality threshold after Fermi.
// For SimpleChain's current 13 validators this is 11 votes.
func optimisticFinalityQuorum(validators int) int {
	return cmath.CeilDiv(validators*4, 5)
}

func isOptimisticFinalityActive(chainConfig *params.ChainConfig, header *types.Header) bool {
	return chainConfig != nil && header != nil && header.Number != nil && chainConfig.IsFermi(header.Number, header.Time)
}

func finalityRewardQuorum(chainConfig *params.ChainConfig, header *types.Header, validators int) int {
	if isOptimisticFinalityActive(chainConfig, header) {
		return optimisticFinalityQuorum(validators)
	}
	return fastFinalityQuorum(validators)
}

func attestationVoteCount(attestation *types.VoteAttestation) int {
	if attestation == nil {
		return 0
	}
	return int(bitset.From([]uint64{uint64(attestation.VoteAddressSet)}).Count())
}
