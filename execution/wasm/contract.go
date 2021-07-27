package wasm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"

	"github.com/go-interpreter/wagon/wasm/leb128"
	lifeExec "github.com/hyperledger/burrow/execution/life/exec"
	hex "github.com/tmthrgd/go-hex"

	bin "github.com/hyperledger/burrow/binary"
	"github.com/hyperledger/burrow/crypto"
	"github.com/hyperledger/burrow/execution/engine"
	"github.com/hyperledger/burrow/execution/errors"
	"github.com/hyperledger/burrow/execution/evm"
	"github.com/hyperledger/burrow/execution/exec"
	"github.com/hyperledger/burrow/permission"
	"github.com/hyperledger/burrow/txs"
)

type Contract struct {
	vm   *WVM
	code []byte
}

const Success = 0
const Error = 1
const Revert = 2

const ValueByteSize = 16

func (c *Contract) Call(state engine.State, params engine.CallParams) (output []byte, err error) {
	return engine.Call(state, params, c.execute)
}

func (c *Contract) execute(state engine.State, params engine.CallParams) ([]byte, error) {
	const errHeader = "ewasm"
	fmt.Println("execute NewVirtualMachine begin: "+time.Now().UTC().String())
	// Since Life runs the execution for us we push the arguments into the import resolver state
	ctx := &context{
		Contract: c,
		state:    state,
		params:   params,
		code:     c.code,
	}

	// panics in ResolveFunc() will be recovered for us, no need for our own
	vm, err := lifeExec.NewVirtualMachine(c.code[0:int(wasmSize(c.code))], c.vm.vmConfig, ctx, nil)
	if err != nil {
		return nil, errors.Errorf(errors.Codes.InvalidContract, "%s: motherfucker %v", errHeader, err)
	}

	fmt.Println("execute NewVirtualMachine end  : "+time.Now().UTC().String())
	entryID, ok := vm.GetFunctionExport("main")
	if !ok {
		return nil, errors.Codes.UnresolvedSymbols
	}


	fmt.Println("vm run begin: "+time.Now().UTC().String())
	_, err = vm.Run(entryID)
	if err != nil && errors.GetCode(err) == errors.Codes.ExecutionReverted {
		return nil, err
	}

	if err != nil && errors.GetCode(err) != errors.Codes.None {
		return nil, errors.Errorf(errors.Codes.ExecutionAborted, "%s: %v", errHeader, err)
	}
	fmt.Println("vm run end  : "+time.Now().UTC().String())

	return ctx.output, nil
}

type context struct {
	*Contract
	state      engine.State
	params     engine.CallParams
	code       []byte
	output     []byte
	returnData []byte
	sequence   uint64
}

var _ lifeExec.ImportResolver = (*context)(nil)

func (ctx *context) ResolveGlobal(module, field string) int64 {
	panic(fmt.Sprintf("global %s module %s not found", field, module))
}

func (ctx *context) ResolveFunc(module, field string) lifeExec.FunctionImport {
	if module == "debug" {
		// See https://github.com/ewasm/hera#interfaces
		switch field {
		case "print32":
			return func(vm *lifeExec.VirtualMachine) int64 {
				n := int32(vm.GetCurrentFrame().Locals[0])

				s := fmt.Sprintf("%d", n)

				err := ctx.state.EventSink.Print(&exec.PrintEvent{
					Address: ctx.params.Callee,
					Data:    []byte(s),
				})

				if err != nil {
					panic(fmt.Sprintf(" => print32 failed: %v", err))
				}

				return Success
			}

		case "print64":
			return func(vm *lifeExec.VirtualMachine) int64 {
				n := int64(vm.GetCurrentFrame().Locals[0])

				s := fmt.Sprintf("%d", n)

				err := ctx.state.EventSink.Print(&exec.PrintEvent{
					Address: ctx.params.Callee,
					Data:    []byte(s),
				})

				if err != nil {
					panic(fmt.Sprintf(" => print32 failed: %v", err))
				}

				return Success
			}

		case "printMem":
			return func(vm *lifeExec.VirtualMachine) int64 {
				dataPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
				dataLen := int(uint32(vm.GetCurrentFrame().Locals[1]))

				s := vm.Memory[dataPtr : dataPtr+dataLen]

				err := ctx.state.EventSink.Print(&exec.PrintEvent{
					Address: ctx.params.Callee,
					Data:    s,
				})

				if err != nil {
					panic(fmt.Sprintf(" => printMem failed: %v", err))
				}

				return Success
			}

		case "printMemHex":
			return func(vm *lifeExec.VirtualMachine) int64 {
				dataPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
				dataLen := int(uint32(vm.GetCurrentFrame().Locals[1]))

				s := hex.EncodeToString(vm.Memory[dataPtr : dataPtr+dataLen])

				err := ctx.state.EventSink.Print(&exec.PrintEvent{
					Address: ctx.params.Callee,
					Data:    []byte(s),
				})

				if err != nil {
					panic(fmt.Sprintf(" => printMemHex failed: %v", err))
				}

				return Success
			}

		case "printStorage":
			return func(vm *lifeExec.VirtualMachine) int64 {
				keyPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

				key := bin.Word256{}

				copy(key[:], vm.Memory[keyPtr:keyPtr+32])

				val, err := ctx.state.GetStorage(ctx.params.Callee, key)
				if err != nil {
					panic(err)
				}

				err = ctx.state.EventSink.Print(&exec.PrintEvent{
					Address: ctx.params.Callee,
					Data:    val,
				})

				if err != nil {
					panic(fmt.Sprintf(" => printStorage failed: %v", err))
				}

				return Success
			}

		case "printStorageHex":
			return func(vm *lifeExec.VirtualMachine) int64 {
				keyPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

				key := bin.Word256{}

				copy(key[:], vm.Memory[keyPtr:keyPtr+32])

				val, err := ctx.state.GetStorage(ctx.params.Callee, key)
				if err != nil {
					panic(err)
				}

				s := hex.EncodeToString(val)

				err = ctx.state.EventSink.Print(&exec.PrintEvent{
					Address: ctx.params.Callee,
					Data:    []byte(s),
				})

				if err != nil {
					panic(fmt.Sprintf(" => printStorage failed: %v", err))
				}

				return Success
			}

		default:
			panic(fmt.Sprintf("function %s unknown for debug module", field))
		}
	}

	if module != "ethereum" {
		panic(fmt.Sprintf("unknown module %s", module))
	}

	switch field {
	case "create":
		return func(vm *lifeExec.VirtualMachine) int64 {
			valuePtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataPtr := uint32(vm.GetCurrentFrame().Locals[1])
			dataLen := uint32(vm.GetCurrentFrame().Locals[2])
			resultPtr := uint32(vm.GetCurrentFrame().Locals[3])

			// TODO: is this guaranteed to be okay? Should be avoid panic here if out of bounds?
			value := bin.BigIntFromLittleEndianBytes(vm.Memory[valuePtr : valuePtr+ValueByteSize])

			var data []byte
			copy(data, vm.Memory[dataPtr:dataPtr+dataLen])

			ctx.sequence++
			nonce := make([]byte, txs.HashLength+8)
			copy(nonce, ctx.vm.options.Nonce)
			binary.BigEndian.PutUint64(nonce[txs.HashLength:], ctx.sequence)
			newAccountAddress := crypto.NewContractAddress(ctx.params.Callee, nonce)

			err := engine.EnsurePermission(ctx.state.CallFrame, ctx.params.Callee, permission.CreateContract)
			if err != nil {
				return Error
			}

			err = ctx.state.CallFrame.CreateAccount(ctx.params.Caller, newAccountAddress)
			if err != nil {
				return Error
			}

			res, err := ctx.vm.Contract(vm.Memory[dataPtr:dataPtr+dataLen]).Call(ctx.state, engine.CallParams{
				Caller: ctx.params.Caller,
				Callee: newAccountAddress,
				Input:  nil,
				Value:  *value,
				Gas:    ctx.params.Gas,
			})

			if err != nil {
				if errors.GetCode(err) == errors.Codes.ExecutionReverted {
					return Revert
				}
				panic(err)
			}
			err = engine.InitWASMCode(ctx.state, newAccountAddress, res)
			if err != nil {
				if errors.GetCode(err) == errors.Codes.ExecutionReverted {
					return Revert
				}
				panic(err)
			}

			copy(vm.Memory[resultPtr:], newAccountAddress.Bytes())

			return Success
		}

	case "getBlockDifficulty":
		return func(vm *lifeExec.VirtualMachine) int64 {
			resultPtr := int(vm.GetCurrentFrame().Locals[0])

			// set it to 1
			copy(vm.Memory[resultPtr:resultPtr+32], bin.RightPadBytes([]byte{1}, 32))
			return Success
		}

	case "getTxGasPrice":
		return func(vm *lifeExec.VirtualMachine) int64 {
			resultPtr := int(vm.GetCurrentFrame().Locals[0])

			// set it to 1
			copy(vm.Memory[resultPtr:resultPtr+16], bin.RightPadBytes([]byte{1}, 16))
			return Success
		}

	case "selfDestruct":
		return func(vm *lifeExec.VirtualMachine) int64 {
			receiverPtr := int(vm.GetCurrentFrame().Locals[0])

			var receiver crypto.Address
			copy(receiver[:], vm.Memory[receiverPtr:receiverPtr+crypto.AddressLength])

			receiverAcc, err := ctx.state.GetAccount(receiver)
			if err != nil {
				panic(err)
			}
			if receiverAcc == nil {
				err := ctx.state.CallFrame.CreateAccount(ctx.params.Callee, receiver)
				if err != nil {
					panic(err)
				}
			}
			acc, err := ctx.state.GetAccount(ctx.params.Callee)
			if err != nil {
				panic(err)
			}
			balance := acc.Balance
			err = acc.AddToBalance(balance)
			if err != nil {
				panic(err)
			}

			err = ctx.state.CallFrame.UpdateAccount(acc)
			if err != nil {
				panic(err)
			}
			err = ctx.state.CallFrame.RemoveAccount(ctx.params.Callee)
			if err != nil {
				panic(err)
			}
			panic(errors.Codes.None)
		}

	case "call", "callCode", "callDelegate", "callStatic":
		return func(vm *lifeExec.VirtualMachine) int64 {
			gasLimit := big.NewInt(vm.GetCurrentFrame().Locals[0])
			addressPtr := uint32(vm.GetCurrentFrame().Locals[1])
			i := 2
			var valuePtr int
			if field == "call" || field == "callCode" {
				valuePtr = int(uint32(vm.GetCurrentFrame().Locals[i]))
				i++
			}
			dataPtr := uint32(vm.GetCurrentFrame().Locals[i])
			dataLen := uint32(vm.GetCurrentFrame().Locals[i+1])

			// TODO: avoid panic? Or at least panic with coded out-of-bounds
			target := crypto.MustAddressFromBytes(vm.Memory[addressPtr : addressPtr+crypto.AddressLength])

			// TODO: is this guaranteed to be okay? Should be avoid panic here if out of bounds?
			value := bin.BigIntFromLittleEndianBytes(vm.Memory[valuePtr : valuePtr+ValueByteSize])

			var callType exec.CallType

			switch field {
			case "call":
				callType = exec.CallTypeCall
			case "callCode":
				callType = exec.CallTypeCode
			case "callStatic":
				callType = exec.CallTypeStatic
			case "callDeletegate":
				callType = exec.CallTypeDelegate
			default:
				panic("should not happen")
			}

			var err error
			ctx.returnData, err = engine.CallFromSite(ctx.state, ctx.vm.externalDispatcher, ctx.params,
				engine.CallParams{
					CallType: callType,
					Callee:   target,
					Input:    vm.Memory[dataPtr : dataPtr+dataLen],
					Value:    *value,
					Gas:      gasLimit,
				})

			// Refund any remaining gas to be used on subsequent calls
			ctx.params.Gas.Add(ctx.params.Gas, gasLimit)

			// TODO[Silas]: we may need to consider trapping and non-trapping errors here in a bit more of a principled way
			//   (e.g. we may be currently handling things that should abort execution, it might be better to clasify
			//   all of our coded errors as trapping (fatal abort WASM) or non-trapping (return error to WASM caller)
			//   I'm not sure this is consistent in EVM either.
			if err != nil {
				if errors.GetCode(err) == errors.Codes.ExecutionReverted {
					return Revert
				}
				// Spec says return 1 for error, but not sure when to do that (as opposed to abort):
				// https://github.com/ewasm/design/blob/master/eth_interface.md#call
				panic(err)
			}
			return Success
		}

	case "getCallDataSize":
		return func(vm *lifeExec.VirtualMachine) int64 {
			return int64(len(ctx.params.Input))
		}

	case "callDataCopy":
		return func(vm *lifeExec.VirtualMachine) int64 {
			destPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataOffset := int(uint32(vm.GetCurrentFrame().Locals[1]))
			dataLen := int(uint32(vm.GetCurrentFrame().Locals[2]))

			if dataLen > 0 {
				copy(vm.Memory[destPtr:], ctx.params.Input[dataOffset:dataOffset+dataLen])
			}

			return Success
		}

	case "getReturnDataSize":
		return func(vm *lifeExec.VirtualMachine) int64 {
			return int64(len(ctx.returnData))
		}

	case "returnDataCopy":
		return func(vm *lifeExec.VirtualMachine) int64 {
			destPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataOffset := int(uint32(vm.GetCurrentFrame().Locals[1]))
			dataLen := int(uint32(vm.GetCurrentFrame().Locals[2]))

			if dataLen > 0 {
				copy(vm.Memory[destPtr:], ctx.returnData[dataOffset:dataOffset+dataLen])
			}

			return Success
		}

	case "getCodeSize":
		return func(vm *lifeExec.VirtualMachine) int64 {
			return int64(len(ctx.code))
		}

	case "codeCopy":
		return func(vm *lifeExec.VirtualMachine) int64 {
			destPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataOffset := int(uint32(vm.GetCurrentFrame().Locals[1]))
			dataLen := int(uint32(vm.GetCurrentFrame().Locals[2]))

			if dataLen > 0 {
				copy(vm.Memory[destPtr:], ctx.code[dataOffset:dataOffset+dataLen])
			}

			return Success
		}

	case "storageStore":
		return func(vm *lifeExec.VirtualMachine) int64 {
			keyPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataPtr := int(uint32(vm.GetCurrentFrame().Locals[1]))

			key := bin.Word256{}
			value := make([]byte, 32)

			copy(key[:], vm.Memory[keyPtr:keyPtr+32])
			copy(value, vm.Memory[dataPtr:dataPtr+32])

			err := ctx.state.SetStorage(ctx.params.Callee, key, value)
			if err != nil {
				panic(err)
			}
			return Success
		}

	case "storageLoad":
		return func(vm *lifeExec.VirtualMachine) int64 {

			keyPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataPtr := int(uint32(vm.GetCurrentFrame().Locals[1]))

			key := bin.Word256{}

			copy(key[:], vm.Memory[keyPtr:keyPtr+32])

			val, err := ctx.state.GetStorage(ctx.params.Callee, key)
			if err != nil {
				panic(err)
			}
			copy(vm.Memory[dataPtr:], val)

			return Success
		}

	case "finish":
		return func(vm *lifeExec.VirtualMachine) int64 {
			dataPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataLen := int(uint32(vm.GetCurrentFrame().Locals[1]))

			ctx.output = vm.Memory[dataPtr : dataPtr+dataLen]

			panic(errors.Codes.None)
		}

	case "revert":
		return func(vm *lifeExec.VirtualMachine) int64 {

			dataPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataLen := int(uint32(vm.GetCurrentFrame().Locals[1]))

			ctx.output = vm.Memory[dataPtr : dataPtr+dataLen]

			panic(errors.Codes.ExecutionReverted)
		}

	case "getAddress":
		return func(vm *lifeExec.VirtualMachine) int64 {
			addressPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

			copy(vm.Memory[addressPtr:], ctx.params.Callee.Bytes())

			return Success
		}

	case "getCallValue":
		return func(vm *lifeExec.VirtualMachine) int64 {
			valuePtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

			// ewasm value is little endian 128 bit value
			copy(vm.Memory[valuePtr:], bin.BigIntToLittleEndianBytes(&ctx.params.Value))

			return Success
		}

	case "getExternalBalance":
		return func(vm *lifeExec.VirtualMachine) int64 {
			addressPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			balancePtr := int(uint32(vm.GetCurrentFrame().Locals[1]))

			address := crypto.Address{}

			copy(address[:], vm.Memory[addressPtr:addressPtr+crypto.AddressLength])
			acc, err := ctx.state.GetAccount(address)
			if err != nil {
				panic(errors.Codes.InvalidAddress)
			}

			// ewasm value is little endian 128 bit value
			bs := make([]byte, 16)
			binary.LittleEndian.PutUint64(bs, acc.Balance)

			copy(vm.Memory[balancePtr:], bs)

			return Success
		}

	case "getBlockTimestamp":
		return func(vm *lifeExec.VirtualMachine) int64 {
			return int64(ctx.state.Blockchain.LastBlockTime().Unix())
		}

	case "getBlockNumber":
		return func(vm *lifeExec.VirtualMachine) int64 {
			return int64(ctx.state.Blockchain.LastBlockHeight())
		}

	case "getTxOrigin":
		return func(vm *lifeExec.VirtualMachine) int64 {
			addressPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

			copy(vm.Memory[addressPtr:addressPtr+crypto.AddressLength], ctx.params.Origin.Bytes())

			return Success
		}

	case "getCaller":
		return func(vm *lifeExec.VirtualMachine) int64 {
			addressPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

			copy(vm.Memory[addressPtr:addressPtr+crypto.AddressLength], ctx.params.Caller.Bytes())

			return Success
		}

	case "getBlockGasLimit":
		return func(vm *lifeExec.VirtualMachine) int64 {
			return ctx.params.Gas.Int64()
		}

	case "getGasLeft":
		return func(vm *lifeExec.VirtualMachine) int64 {
			// do the same as EVM
			return ctx.params.Gas.Int64()
		}

	case "getBlockCoinbase":
		return func(vm *lifeExec.VirtualMachine) int64 {
			// do the same as EVM
			addressPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))

			copy(vm.Memory[addressPtr:addressPtr+crypto.AddressLength], crypto.ZeroAddress.Bytes())

			return Success
		}

	case "getBlockHash":
		return func(vm *lifeExec.VirtualMachine) int64 {
			blockNumber := uint64(vm.GetCurrentFrame().Locals[0])
			hashPtr := int(vm.GetCurrentFrame().Locals[1])

			lastBlockHeight := ctx.state.Blockchain.LastBlockHeight()
			if blockNumber >= lastBlockHeight {
				panic(fmt.Sprintf(" => attempted to get block hash of a non-existent block: %v", blockNumber))
			} else if lastBlockHeight-blockNumber > evm.MaximumAllowedBlockLookBack {
				panic(fmt.Sprintf(" => attempted to get block hash of a block %d outside of the allowed range "+
					"(must be within %d blocks)", blockNumber, evm.MaximumAllowedBlockLookBack))
			} else {
				hash, err := ctx.state.Blockchain.BlockHash(blockNumber)
				if err != nil {
					panic(fmt.Sprintf(" => blockhash failed: %v", err))
				}

				copy(vm.Memory[hashPtr:hashPtr+len(hash)], hash)
			}

			return Success
		}

	case "log":
		return func(vm *lifeExec.VirtualMachine) int64 {
			dataPtr := int(uint32(vm.GetCurrentFrame().Locals[0]))
			dataLen := int(uint32(vm.GetCurrentFrame().Locals[1]))

			data := vm.Memory[dataPtr : dataPtr+dataLen]

			topicCount := uint32(vm.GetCurrentFrame().Locals[2])
			topics := make([]bin.Word256, topicCount)

			if topicCount > 4 {
				panic(fmt.Sprintf("%d topics not permitted", topicCount))
			}

			for i := uint32(0); i < topicCount; i++ {
				topicPtr := int(uint32(vm.GetCurrentFrame().Locals[3+i]))
				topicData := vm.Memory[topicPtr : topicPtr+bin.Word256Bytes]
				topics[i] = bin.RightPadWord256(topicData)
			}

			err := ctx.state.EventSink.Log(&exec.LogEvent{
				Address: ctx.params.Callee,
				Topics:  topics,
				Data:    data,
			})

			if err != nil {
				panic(fmt.Sprintf(" => log failed: %v", err))
			}

			return Success
		}

	default:
		panic(fmt.Sprintf("unknown function %s", field))
	}
}

// When deploying wasm code, the abi encoded arguments to the constructor are added to the code. Wagon
// does not like seeing this data, so strip this off. We have to walk the wasm format to the last section

// There might be a better solution to this.
func wasmSize(code []byte) int64 {
	reader := bytes.NewReader(code)
	top := int64(8)
	for {
		reader.Seek(top, 0)
		id, err := reader.ReadByte()
		if err != nil || id == 0 || id > 11 {
			// invalid section id
			break
		}
		size, err := leb128.ReadVarUint32(reader)
		if err != nil {
			break
		}
		pos, _ := reader.Seek(0, 1)
		if pos+int64(size) > int64(len(code)) {
			break
		}
		top = pos + int64(size)
	}

	return top
}
