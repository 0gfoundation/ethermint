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
	"sort"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/evmos/ethermint/x/feemarket/types"

	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/x/auth/signing"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
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

	k.foundSuggestionGasPrice(ctx)
}

func (k *Keeper) foundSuggestionGasPrice(ctx sdk.Context) {
	logger := k.Logger(ctx)
	var maxBlockGas uint64
	if b := ctx.ConsensusParams().Block; b != nil {
		maxBlockGas = uint64(b.MaxGas)

		txnInfoMap := make(map[string][]*txnInfo, k.mempool.CountTx())
		iterator := k.mempool.Select(ctx, nil)

		for iterator != nil {
			memTx := iterator.Tx()

			sigVerifiableTx, ok := memTx.(signing.SigVerifiableTx)
			if !ok {
				logger.Error("memTx is not a SigVerifiableTx: ", "type", reflect.TypeOf(memTx))
				iterator = iterator.Next()
				continue
			}

			sigs, err := sigVerifiableTx.GetSignaturesV2()
			if err != nil {
				logger.Error("failed to get signatures:", "error=", err)
				iterator = iterator.Next()
				continue
			}

			if len(sigs) == 0 {
				msgs := memTx.GetMsgs()
				if len(msgs) == 1 {
					msgEthTx, ok := msgs[0].(*evmtypes.MsgEthereumTx)
					if ok {
						ethTx := msgEthTx.AsTransaction()
						signer := gethtypes.NewEIP2930Signer(ethTx.ChainId())
						ethSender, err := signer.Sender(ethTx)
						if err == nil {
							signer := sdk.AccAddress(ethSender.Bytes()).String()
							nonce := ethTx.Nonce()

							if _, exists := txnInfoMap[signer]; !exists {
								txnInfoMap[signer] = make([]*txnInfo, 0, 128)
							}

							txnInfoMap[signer] = append(txnInfoMap[signer], &txnInfo{
								gasPrice: ethTx.GasPrice(),
								gasLimit: ethTx.Gas(),
								nonce:    nonce,
								sender:   signer,
							})
						}
					}
				}
				// ignore cosmos txn case now
				// } else {
				// 	// ignore multisig case now
				// 	if fee, ok := memTx.(sdk.FeeTx); ok {
				// 		feeCoins := fee.GetFee()
				// 		if len(feeCoins) != 0 {
				// 			if len(sigs) == 1 {
				// 				signer := sdk.AccAddress(sigs[0].PubKey.Address()).String()

				// 				if _, exists := txnInfoMap[signer]; !exists {
				// 					txnInfoMap[signer] = make([]*txnInfo, 0, 16)
				// 				}

				// 				gasPrice := sdk.NewDecCoinsFromCoins(fee.GetFee()...).QuoDec(math.LegacyNewDec(int64(fee.GetGas())))

				// 				evmGasPrice, err := utilCosmosDemonGasPriceToEvmDemonGasPrice(gasPrice)
				// 				evmGasLimit := utilCosmosDemonGasLimitToEvmDemonGasLimit(fee.GetGas())

				// 				if err == nil {
				// 					txnInfoMap[signer] = append(txnInfoMap[signer], &txnInfo{
				// 						gasPrice: evmGasPrice,
				// 						gasLimit: evmGasLimit,
				// 						nonce:    sigs[0].Sequence,
				// 						sender:   signer,
				// 					})
				// 				}
				// 			}
				// 		}
				// 	} else {
				// 		logger.Error("unknown type of memTx: ", "type", reflect.TypeOf(memTx))
				// 	}
			}

			iterator = iterator.Next()
		}

		logger.Debug("mempool size: ", "size", k.mempool.CountTx())
		if len(txnInfoMap) == 0 {
			logger.Debug("not found suggestion gas price!")
			k.SetSuggestionGasPrice(ctx, big.NewInt(0))
		} else {
			senderCnt := 0
			txnCnt := 0
			for sender := range txnInfoMap {
				sort.Slice(txnInfoMap[sender], func(i, j int) bool {
					return txnInfoMap[sender][i].nonce < txnInfoMap[sender][j].nonce
				})
				txnCnt += len(txnInfoMap[sender])
				senderCnt++
			}

			remaing := gasPriceSuggestionBlockNum * int64(maxBlockGas)
			var lastProcessedTx *txnInfo

			for remaing > 0 && len(txnInfoMap) > 0 {
				// Find the highest gas price among the first transaction of each account
				var highestGasPrice *big.Int
				var selectedSender string

				// Compare first transaction (lowest nonce) from each account
				for sender, txns := range txnInfoMap {
					if len(txns) == 0 {
						delete(txnInfoMap, sender)
						continue
					}

					// First tx has lowest nonce due to earlier sorting
					if highestGasPrice == nil || txns[0].gasPrice.Cmp(highestGasPrice) > 0 {
						highestGasPrice = txns[0].gasPrice
						selectedSender = sender
					}
				}

				if selectedSender == "" {
					break
				}

				// Process the selected transaction
				selectedTx := txnInfoMap[selectedSender][0]
				remaing -= int64(selectedTx.gasLimit)
				lastProcessedTx = selectedTx

				// Remove processed transaction
				txnInfoMap[selectedSender] = txnInfoMap[selectedSender][1:]
				if len(txnInfoMap[selectedSender]) == 0 {
					delete(txnInfoMap, selectedSender)
				}
			}

			if lastProcessedTx != nil && remaing <= 0 {
				logger.Debug("found suggestion gas price: ", "value", lastProcessedTx.gasPrice.String())
				k.SetSuggestionGasPrice(ctx, lastProcessedTx.gasPrice)
			} else {
				logger.Debug("not found suggestion gas price!")
				k.SetSuggestionGasPrice(ctx, big.NewInt(0))
			}
		}
	}
}

func utilCosmosDemonGasPriceToEvmDemonGasPrice(gasGroup sdk.DecCoins) (*big.Int, error) {
	gasPrice := big.NewInt(0)
	for _, coin := range gasGroup {
		if coin.Denom == GasDenom {
			thisGasPrice := coin.Amount.MulRoundUp(sdk.NewDec(GasDenomConversionMultiplier))
			gasPrice = gasPrice.Add(gasPrice, thisGasPrice.TruncateInt().BigInt())
		} else {
			return big.NewInt(0), fmt.Errorf("invalid denom: %s", coin.Denom)
		}
	}

	return gasPrice, nil
}

func utilCosmosDemonGasLimitToEvmDemonGasLimit(gasLimit uint64) uint64 {
	return gasLimit * GasDenomConversionMultiplier
}
