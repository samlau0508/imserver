package server

import (
	"sync"

	okproto "github.com/samlau0508/imserver/pkg/proto"
)

type FramePool struct {
	sendPacketsPool    sync.Pool
	recvackPacketsPool sync.Pool
	subPacketsPool     sync.Pool
	subackPacketsPool  sync.Pool
}

func NewFramePool() *FramePool {

	return &FramePool{
		sendPacketsPool: sync.Pool{
			New: func() any {
				return make([]*okproto.SendPacket, 0)
			},
		},
		recvackPacketsPool: sync.Pool{
			New: func() any {
				return make([]*okproto.RecvackPacket, 0)
			},
		},
		subPacketsPool: sync.Pool{
			New: func() any {
				return make([]*okproto.SubPacket, 0)
			},
		},
		subackPacketsPool: sync.Pool{
			New: func() any {
				return make([]*okproto.SubackPacket, 0)
			},
		},
	}
}

func (f *FramePool) GetSendPackets() []*okproto.SendPacket {
	return f.sendPacketsPool.Get().([]*okproto.SendPacket)
}

func (f *FramePool) PutSendPackets(sendPackets []*okproto.SendPacket) {
	s := sendPackets[:0]
	f.sendPacketsPool.Put(s)
}

func (f *FramePool) GetRecvackPackets() []*okproto.RecvackPacket {
	return f.recvackPacketsPool.Get().([]*okproto.RecvackPacket)
}
func (f *FramePool) PutRecvackPackets(recvackPackets []*okproto.RecvackPacket) {
	s := recvackPackets[:0]
	f.recvackPacketsPool.Put(s)
}

func (f *FramePool) GetSubPackets() []*okproto.SubPacket {
	return f.subPacketsPool.Get().([]*okproto.SubPacket)
}
func (f *FramePool) PutSubPackets(subPackets []*okproto.SubPacket) {
	s := subPackets[:0]
	f.subPacketsPool.Put(s)
}
func (f *FramePool) GetSubackPackets() []*okproto.SubackPacket {
	return f.subackPacketsPool.Get().([]*okproto.SubackPacket)
}

func (f *FramePool) PutSubackPackets(subackPackets []*okproto.SubackPacket) {
	s := subackPackets[:0]
	f.subackPacketsPool.Put(s)
}
