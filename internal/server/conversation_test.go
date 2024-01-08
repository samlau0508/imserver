package server

import (
	"testing"
	"time"

	okproto "github.com/samlau0508/imserver/pkg/proto"
	"github.com/stretchr/testify/assert"
)

func TestGetConversations(t *testing.T) {
	opts := NewTestOptions()
	opts.Conversation.SyncOnce = 0
	l := NewTestServer(opts)
	cm := NewConversationManager(l)
	cm.Start()

	defer cm.Stop()
	m := &Message{
		RecvPacket: &okproto.RecvPacket{
			Framer: okproto.Framer{
				RedDot: true,
			},
			MessageID:   123,
			ChannelID:   "group1",
			ChannelType: 2,
			FromUID:     "test",
			Timestamp:   int32(time.Now().Unix()),
			Payload:     []byte("hello"),
		},
	}
	cm.PushMessage(m, []string{"test"})

	m = &Message{
		RecvPacket: &okproto.RecvPacket{
			Framer: okproto.Framer{
				RedDot: true,
			},
			MessageID:   123,
			ChannelID:   "group2",
			ChannelType: 2,
			FromUID:     "test",
			Timestamp:   int32(time.Now().Unix()),
			Payload:     []byte("hello"),
		},
	}
	cm.PushMessage(m, []string{"test"})

	time.Sleep(time.Millisecond * 100) // wait calc conversation

	conversations := cm.GetConversations("test", 0, nil)
	assert.Equal(t, 2, len(conversations))

	cm.s.store.Close()

}
