package hotstuff

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// GetSnapshot retrieves the authorization snapshot at a block number.
func (api *API) GetSnapshot(number *rpc.BlockNumber) (*Snapshot, error) {
	header := api.getHeader(number)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.parlia.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
}

func (api *API) GetSnapshotAtHash(hash common.Hash) (*Snapshot, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.parlia.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
}

func (api *API) GetValidators(number *rpc.BlockNumber) ([]common.Address, error) {
	snap, err := api.GetSnapshot(number)
	if err != nil {
		return nil, err
	}
	return snap.validators(), nil
}

func (api *API) GetValidatorsAtHash(hash common.Hash) ([]common.Address, error) {
	snap, err := api.GetSnapshotAtHash(hash)
	if err != nil {
		return nil, err
	}
	return snap.validators(), nil
}

func (api *API) GetJustifiedNumber(number *rpc.BlockNumber) (uint64, error) {
	header := api.getHeader(number)
	if header == nil {
		return 0, errUnknownBlock
	}
	if !api.chain.Config().IsHotstuff(header.Number) {
		snap, err := api.parlia.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
		if err != nil || snap.Attestation == nil {
			return 0, err
		}
		return snap.Attestation.TargetNumber, nil
	}
	justified, _, err := api.parlia.GetJustifiedNumberAndHash(api.chain, []*types.Header{header})
	return justified, err
}

func (api *API) GetTurnLength(number *rpc.BlockNumber) (uint8, error) {
	snap, err := api.GetSnapshot(number)
	if err != nil || snap.TurnLength == 0 {
		return 0, err
	}
	return snap.TurnLength, nil
}

func (api *API) GetFinalizedNumber(number *rpc.BlockNumber) (uint64, error) {
	header := api.getHeader(number)
	if header == nil {
		return 0, errUnknownBlock
	}
	if !api.chain.Config().IsHotstuff(header.Number) {
		snap, err := api.parlia.snapshot(api.chain, header.Number.Uint64(), header.Hash(), nil)
		if err != nil || snap.Attestation == nil {
			return 0, err
		}
		return snap.Attestation.SourceNumber, nil
	}
	finalized := api.parlia.GetFinalizedHeader(api.chain, header)
	if finalized == nil {
		return 0, nil
	}
	return finalized.Number.Uint64(), nil
}

func (api *API) getHeader(number *rpc.BlockNumber) *types.Header {
	current := api.chain.CurrentHeader()
	switch {
	case number == nil || *number == rpc.LatestBlockNumber:
		return current
	case *number == rpc.SafeBlockNumber:
		if current == nil {
			return nil
		}
		if !api.chain.Config().IsHotstuff(current.Number) {
			snap, err := api.parlia.snapshot(api.chain, current.Number.Uint64(), current.Hash(), nil)
			if err != nil || snap.Attestation == nil {
				return api.chain.GetHeaderByNumber(0)
			}
			return api.chain.GetHeader(snap.Attestation.TargetHash, snap.Attestation.TargetNumber)
		}
		number, hash, err := api.parlia.GetJustifiedNumberAndHash(api.chain, []*types.Header{current})
		if err != nil {
			return nil
		}
		return api.chain.GetHeader(hash, number)
	case *number == rpc.FinalizedBlockNumber:
		if current == nil {
			return nil
		}
		if !api.chain.Config().IsHotstuff(current.Number) {
			snap, err := api.parlia.snapshot(api.chain, current.Number.Uint64(), current.Hash(), nil)
			if err != nil || snap.Attestation == nil {
				return api.chain.GetHeaderByNumber(0)
			}
			return api.chain.GetHeader(snap.Attestation.SourceHash, snap.Attestation.SourceNumber)
		}
		return api.parlia.GetFinalizedHeader(api.chain, current)
	case *number == rpc.PendingBlockNumber:
		return nil
	case *number == rpc.EarliestBlockNumber:
		return api.chain.GetHeaderByNumber(0)
	default:
		return api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
}
