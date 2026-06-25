// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package bsc

import (
	metrics "github.com/ethereum/go-ethereum/metrics"
)

var (
	ingressRegistrationErrorName = "eth/protocols/bsc/ingress/registration/error"
	egressRegistrationErrorName  = "eth/protocols/bsc/egress/registration/error"

	IngressRegistrationErrorMeter = metrics.NewRegisteredMeter(ingressRegistrationErrorName, nil)
	EgressRegistrationErrorMeter  = metrics.NewRegisteredMeter(egressRegistrationErrorName, nil)

	BlockChunkPathMeter        = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/path", nil)
	BlockChunkFallbackMeter    = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/fallback", nil)
	BlockChunkShardInMeter     = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/shard/in", nil)
	BlockChunkShardOutMeter    = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/shard/out", nil)
	BlockChunkShardDropMeter   = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/shard/drop", nil)
	BlockChunkReassembleTimer  = metrics.NewRegisteredTimer("eth/protocols/bsc/chunk/reassemble/time", nil)
	BlockChunkReassembleMeter  = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/reassemble/success", nil)
	BlockChunkReassembleErrors = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/reassemble/error", nil)
	BlockChunkMissingReqMeter  = metrics.NewRegisteredMeter("eth/protocols/bsc/chunk/missing/request", nil)
	BlockChunkFanoutGauge      = metrics.NewRegisteredGauge("eth/protocols/bsc/chunk/fanout", nil)
	BlockChunkPeerGauge        = metrics.NewRegisteredGauge("eth/protocols/bsc/chunk/peers", nil)
)
