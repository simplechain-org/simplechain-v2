package hotstuff

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/parlia"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// transitionAPI keeps the historical parlia RPC namespace while routing each
// query to the engine that owns the requested block. Registering both child
// APIs under the same namespace would make duplicate methods order-dependent.
type transitionAPI struct {
	chain       consensus.ChainHeaderReader
	transition  *Transition
	parliaAPI   *parlia.API
	hotstuffAPI *API
}

func (api *transitionAPI) GetSnapshot(number *rpc.BlockNumber) (any, error) {
	header := api.getHeader(number)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.getSnapshotAtHeader(header)
}

func (api *transitionAPI) GetSnapshotAtHash(hash common.Hash) (any, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.getSnapshotAtHeader(header)
}

func (api *transitionAPI) GetValidators(number *rpc.BlockNumber) ([]common.Address, error) {
	header := api.getHeader(number)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.getValidatorsAtHeader(header)
}

func (api *transitionAPI) GetValidatorsAtHash(hash common.Hash) ([]common.Address, error) {
	header := api.chain.GetHeaderByHash(hash)
	if header == nil {
		return nil, errUnknownBlock
	}
	return api.getValidatorsAtHeader(header)
}

func (api *transitionAPI) GetJustifiedNumber(number *rpc.BlockNumber) (uint64, error) {
	header := api.getHeader(number)
	if header == nil {
		return 0, errUnknownBlock
	}
	justified, _, err := api.transition.GetJustifiedNumberAndHash(api.chain, []*types.Header{header})
	return justified, err
}

func (api *transitionAPI) GetTurnLength(number *rpc.BlockNumber) (uint8, error) {
	header := api.getHeader(number)
	if header == nil {
		return 0, errUnknownBlock
	}
	snapshot, err := api.getSnapshotAtHeader(header)
	if err != nil {
		return 0, err
	}
	switch snapshot := snapshot.(type) {
	case *Snapshot:
		return snapshot.TurnLength, nil
	case *parlia.Snapshot:
		return snapshot.TurnLength, nil
	default:
		return 0, errUnknownBlock
	}
}

func (api *transitionAPI) GetFinalizedNumber(number *rpc.BlockNumber) (uint64, error) {
	header := api.getHeader(number)
	if header == nil {
		return 0, errUnknownBlock
	}
	finalized := api.transition.GetFinalizedHeader(api.chain, header)
	if finalized == nil {
		return 0, nil
	}
	return finalized.Number.Uint64(), nil
}

func (api *transitionAPI) getSnapshotAtHeader(header *types.Header) (any, error) {
	if api.isHotstuff(header) {
		return api.hotstuffAPI.GetSnapshotAtHash(header.Hash())
	}
	return api.parliaAPI.GetSnapshotAtHash(header.Hash())
}

func (api *transitionAPI) getValidatorsAtHeader(header *types.Header) ([]common.Address, error) {
	if api.isHotstuff(header) {
		return api.hotstuffAPI.GetValidatorsAtHash(header.Hash())
	}
	return api.parliaAPI.GetValidatorsAtHash(header.Hash())
}

func (api *transitionAPI) getHeader(number *rpc.BlockNumber) *types.Header {
	current := api.chain.CurrentHeader()
	switch {
	case number == nil || *number == rpc.LatestBlockNumber:
		return current
	case *number == rpc.SafeBlockNumber:
		if current == nil {
			return nil
		}
		number, hash, err := api.transition.GetJustifiedNumberAndHash(api.chain, []*types.Header{current})
		if err != nil {
			return nil
		}
		return api.chain.GetHeader(hash, number)
	case *number == rpc.FinalizedBlockNumber:
		if current == nil {
			return nil
		}
		return api.transition.GetFinalizedHeader(api.chain, current)
	case *number == rpc.PendingBlockNumber:
		return nil
	case *number == rpc.EarliestBlockNumber:
		return api.chain.GetHeaderByNumber(0)
	default:
		return api.chain.GetHeaderByNumber(uint64(number.Int64()))
	}
}

func (api *transitionAPI) isHotstuff(header *types.Header) bool {
	return api.chain.Config().IsHotstuff(header.Number)
}
