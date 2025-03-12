// Copyright 2021 Evmos Foundation
// This file is part of Evmos' Ethermint library.
//
// The Ethermint library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The Ethermint library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the Ethermint library. If not, see https://github.com/evmos/ethermint/blob/main/LICENSE
package keeper

import (
	"fmt"
	"math/big"
	"reflect"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/evmos/ethermint/x/feemarket/types"

	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/mempool"
	"github.com/cosmos/cosmos-sdk/x/auth/signing"
	evmtypes "github.com/evmos/ethermint/x/evm/types"
)

const (
	gasPriceSuggestionBlockNum   int64 = 5
	GasDenom                           = "ua0gi"
	GasDenomConversionMultiplier       = 1e12
)

type txnInfo struct {
	gasPrice *big.Int
	gasLimit uint64
	nonce    uint64
	sender   string
}

// BeginBlock updates base fee
func (k *Keeper) BeginBlock(ctx sdk.Context, req abci.RequestBeginBlock) { //nolint: revive
	baseFee := k.CalculateBaseFee(ctx)

	// return immediately if base fee is nil
	if baseFee == nil {
		return
	}

	k.SetBaseFee(ctx, baseFee)

	defer func() {
		telemetry.SetGauge(float32(baseFee.Int64()), "feemarket", "base_fee")
	}()

	// Store current base fee in event
	ctx.EventManager().EmitEvents(sdk.Events{
		sdk.NewEvent(
			types.EventTypeFeeMarket,
			sdk.NewAttribute(types.AttributeKeyBaseFee, baseFee.String()),
		),
	})
}

// EndBlock update block gas wanted.
// The EVM end block logic doesn't update the validator set, thus it returns
// an empty slice.
func (k *Keeper) EndBlock(ctx sdk.Context, req abci.RequestEndBlock) { //nolint: revive
	if ctx.BlockGasMeter() == nil {
		k.Logger(ctx).Error("block gas meter is nil when setting block gas wanted")
		return
	}

	gasWanted := k.GetTransientGasWanted(ctx)
	gasUsed := ctx.BlockGasMeter().GasConsumedToLimit()

	// to prevent BaseFee manipulation we limit the gasWanted so that
	// gasWanted = max(gasWanted * MinGasMultiplier, gasUsed)
	// this will be keep BaseFee protected from un-penalized manipulation
	// more info here https://github.com/evmos/ethermint/pull/1105#discussion_r888798925
	minGasMultiplier := k.GetParams(ctx).MinGasMultiplier
	limitedGasWanted := sdk.NewDec(int64(gasWanted)).Mul(minGasMultiplier)
	gasWanted = sdk.MaxDec(limitedGasWanted, sdk.NewDec(int64(gasUsed))).TruncateInt().Uint64()
	k.SetBlockGasWanted(ctx, gasWanted)

	defer func() {
		telemetry.SetGauge(float32(gasWanted), "feemarket", "block_gas")
	}()

	ctx.EventManager().EmitEvent(sdk.NewEvent(
		"block_gas",
		sdk.NewAttribute("height", fmt.Sprintf("%d", ctx.BlockHeight())),
		sdk.NewAttribute("amount", fmt.Sprintf("%d", gasWanted)),
	))
	startAt := time.Now()
	k.foundSuggestionGasPrice(ctx)
	k.Logger(ctx).Info("found suggestion gas price", "costed", fmt.Sprint(time.Since(startAt).Milliseconds()))
}

func (k *Keeper) foundSuggestionGasPrice(ctx sdk.Context) {
	logger := k.Logger(ctx)
	var maxBlockGas uint64
	if b := ctx.ConsensusParams().Block; b != nil {
		maxBlockGas = uint64(b.MaxGas)
		remaing := gasPriceSuggestionBlockNum * int64(maxBlockGas)
		var lastGasPrice *big.Int
		txCnt := 0
		mempool.SelectBy(ctx.Context(), k.mempool, [][]byte{}, func(memTx sdk.Tx) bool {
			sigVerifiableTx, ok := memTx.(signing.SigVerifiableTx)
			if !ok {
				logger.Error("memTx is not a SigVerifiableTx: ", "type", reflect.TypeOf(memTx))
			} else {
				sigs, err := sigVerifiableTx.GetSignaturesV2()
				if err != nil {
					logger.Error("failed to get signatures:", "error=", err)
				} else {
					if len(sigs) == 0 {
						msgs := memTx.GetMsgs()
						if len(msgs) == 1 {
							msgEthTx, ok := msgs[0].(*evmtypes.MsgEthereumTx)
							if ok {
								ethTx := msgEthTx.AsTransaction()
								remaing -= int64(ethTx.Gas())
								if remaing <= 0 {
									lastGasPrice = ethTx.GasPrice()
									return false
								}
								txCnt++
							}
						}
					}
				}
			}

			return true
		})

		if lastGasPrice != nil {
			logger.Info("found suggestion gas price: ", "value", lastGasPrice.String(), "txCnt", txCnt)
			k.SetSuggestionGasPrice(ctx, lastGasPrice)
		} else {
			logger.Info("not found suggestion gas price!")
			k.SetSuggestionGasPrice(ctx, big.NewInt(0))
		}
	}
}
