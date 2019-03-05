package dex

import (
	"github.com/golang/protobuf/proto"
	"github.com/vitelabs/go-vite/common"
	"github.com/vitelabs/go-vite/common/types"
	dexproto "github.com/vitelabs/go-vite/vm/contracts/dex/proto"
)

const orderEventName = "orderUpdateEvent"
const newOrderFailEventName = "newOrderFailEvent"
const txEventName = "txEvent"


const (
	NewOrderGetFundFail = iota
	NewOrderLockFundFail
	NewOrderSaveFundFail
	NewOrderInternalErr
)

type OrderEvent interface {
	getTopicId() types.Hash
	toDataBytes() []byte
	fromBytes([]byte) interface{}
}

type OrderUpdateEvent struct {
	dexproto.Order
}

type TransactionEvent struct {
	dexproto.Transaction
}

type NewOrderFailEvent struct {
	dexproto.OrderFail
}

func (od OrderUpdateEvent) getTopicId() types.Hash {
	return fromNameToHash(orderEventName)
}

func (od OrderUpdateEvent) toDataBytes() []byte {
	data, _ := proto.Marshal(&od.Order)
	return data
}

func (od OrderUpdateEvent) fromBytes(data []byte) interface{} {
	event := OrderUpdateEvent{}
	if err := proto.Unmarshal(data, &event.Order); err != nil {
		return nil
	} else {
		return event
	}
}

func (tx TransactionEvent) getTopicId() types.Hash {
	return fromNameToHash(txEventName)
}

func (tx TransactionEvent) toDataBytes() []byte {
	data, _ := proto.Marshal(&tx.Transaction)
	return data
}

func (tx TransactionEvent) fromBytes(data []byte) interface{} {
	event := TransactionEvent{}
	if err := proto.Unmarshal(data, &event.Transaction); err != nil {
		return nil
	} else {
		return event
	}
}

func (of NewOrderFailEvent) getTopicId() types.Hash {
	return fromNameToHash(newOrderFailEventName)
}

func (of NewOrderFailEvent) toDataBytes() []byte {
	data, _ := proto.Marshal(&of.OrderFail)
	return data
}

func (of NewOrderFailEvent) fromBytes(data []byte) interface{} {
	event := NewOrderFailEvent{}
	if err := proto.Unmarshal(data, &event.OrderFail); err != nil {
		return nil
	} else {
		return event
	}
}

func fromNameToHash(name string) types.Hash {
	hs := types.Hash{}
	hs.SetBytes(common.RightPadBytes([]byte(name), types.HashSize))
	return hs
}