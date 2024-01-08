package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/snowflake"
	"github.com/samlau0508/imserver/pkg/oklog"
	"github.com/samlau0508/imserver/pkg/oknet"
	"github.com/samlau0508/imserver/pkg/okstore"
	"github.com/samlau0508/imserver/pkg/okutil"
	okproto "github.com/samlau0508/imserver/pkg/proto"
	"go.uber.org/zap"
)

type Processor struct {
	s               *Server
	connContextPool sync.Pool
	frameWorkPool   *FrameWorkPool
	oklog.Log
	messageIDGen *snowflake.Node // 消息ID生成器

	framePool *FramePool // 对象池
}

func NewProcessor(s *Server) *Processor {
	// Initialize the messageID generator of the snowflake algorithm
	messageIDGen, err := snowflake.NewNode(int64(s.opts.ID))
	if err != nil {
		panic(err)
	}
	return &Processor{
		s:             s,
		messageIDGen:  messageIDGen,
		Log:           oklog.NewOKLog("Processor"),
		frameWorkPool: NewFrameWorkPool(),
		framePool:     NewFramePool(),
		connContextPool: sync.Pool{
			New: func() any {
				cc := newConnContext(s)
				cc.init()
				return cc
			},
		},
	}
}

// 处理同类型的frame集合
func (p *Processor) processSameFrame(conn oknet.Conn, frameType okproto.FrameType, frames []okproto.Frame, s, e int) {
	switch frameType {
	case okproto.PING: // ping
		p.processPing(conn, frames[0].(*okproto.PingPacket))
	case okproto.SEND: // process send
		tmpFrames := p.framePool.GetSendPackets()
		for _, frame := range frames {
			tmpFrames = append(tmpFrames, frame.(*okproto.SendPacket))
		}
		p.processMsgs(conn, tmpFrames)

		p.framePool.PutSendPackets(tmpFrames) // 注意：这里回收了，processMsgs里不要对tmpFrames进行异步操作，容易引起数据错乱

	case okproto.RECVACK: // process recvack
		tmpFrames := p.framePool.GetRecvackPackets()
		for _, frame := range frames {
			tmpFrames = append(tmpFrames, frame.(*okproto.RecvackPacket))
		}
		p.processRecvacks(conn, tmpFrames)

		p.framePool.PutRecvackPackets(tmpFrames)

	case okproto.SUB: // 订阅
		tmpFrames := p.framePool.GetSubPackets()
		for _, frame := range frames {
			tmpFrames = append(tmpFrames, frame.(*okproto.SubPacket))
		}
		p.processSubs(conn, tmpFrames)
		p.framePool.PutSubPackets(tmpFrames)
	}
	// 完成frame处理
	conn.Context().(*connContext).finishFrames(len(frames))

}

// #################### conn auth ####################
func (p *Processor) processAuth(conn oknet.Conn, connectPacket *okproto.ConnectPacket) {
	var (
		uid                             = connectPacket.UID
		devceLevel  okproto.DeviceLevel = okproto.DeviceLevelMaster
		err         error
		devceLevelI uint8
		token       string
	)
	if strings.TrimSpace(connectPacket.ClientKey) == "" {
		p.responseConnackAuthFail(conn)
		return
	}
	// -------------------- token verify --------------------
	if connectPacket.UID == p.s.opts.ManagerUID {
		if p.s.opts.ManagerTokenOn && connectPacket.Token != p.s.opts.ManagerToken {
			p.Error("manager token verify fail", zap.String("uid", uid), zap.String("token", connectPacket.Token))
			p.responseConnackAuthFail(conn)
			return
		}
		devceLevel = okproto.DeviceLevelSlave // 默认都是slave设备
	} else if p.s.opts.TokenAuthOn {
		if connectPacket.Token == "" {
			p.Error("token is empty")
			p.responseConnackAuthFail(conn)
			return
		}
		token, devceLevelI, err = p.s.store.GetUserToken(uid, connectPacket.DeviceFlag.ToUint8())
		if err != nil {
			p.Error("get user token err", zap.Error(err))
			p.responseConnackAuthFail(conn)
			return
		}
		if token != connectPacket.Token {
			p.Error("token verify fail", zap.String("expectToken", token), zap.String("actToken", connectPacket.Token), zap.Any("conn", conn))
			p.responseConnackAuthFail(conn)
			return
		}
		devceLevel = okproto.DeviceLevel(devceLevelI)
	} else {
		devceLevel = okproto.DeviceLevelSlave // 默认都是slave设备
	}

	// -------------------- ban  --------------------
	userChannelInfo, err := p.s.store.GetChannel(uid, okproto.ChannelTypePerson)
	if err != nil {
		p.Error("get user channel info err", zap.Error(err))
		p.responseConnackAuthFail(conn)
		return
	}
	ban := false
	if userChannelInfo != nil {
		ban = userChannelInfo.Ban
	}
	if ban {
		p.Error("user is ban", zap.String("uid", uid))
		p.responseConnack(conn, 0, okproto.ReasonBan)
		return
	}

	// -------------------- get message encrypt key --------------------
	dhServerPrivKey, dhServerPublicKey := okutil.GetCurve25519KeypPair() // 生成服务器的DH密钥对
	aesKey, aesIV, err := p.getClientAesKeyAndIV(connectPacket.ClientKey, dhServerPrivKey)
	if err != nil {
		p.Error("get client aes key and iv err", zap.Error(err))
		p.responseConnackAuthFail(conn)
		return
	}
	dhServerPublicKeyEnc := base64.StdEncoding.EncodeToString(dhServerPublicKey[:])

	// -------------------- same master kicks each other --------------------
	oldConns := p.s.connManager.GetConnsWith(uid, connectPacket.DeviceFlag)
	if len(oldConns) > 0 {
		if devceLevel == okproto.DeviceLevelMaster { // 如果设备是master级别，则把旧连接都踢掉
			for _, oldConn := range oldConns {
				p.s.connManager.RemoveConnWithID(oldConn.ID())
				if oldConn.DeviceID() != connectPacket.DeviceID {
					p.Info("same master kicks each other", zap.String("devceLevel", devceLevel.String()), zap.String("uid", uid), zap.String("deviceID", connectPacket.DeviceID), zap.String("oldDeviceID", oldConn.DeviceID()))
					p.response(oldConn, &okproto.DisconnectPacket{
						ReasonCode: okproto.ReasonConnectKick,
						Reason:     "login in other device",
					})
					p.s.timingWheel.AfterFunc(time.Second*5, func() {
						oldConn.Close()
					})
				} else {
					p.s.timingWheel.AfterFunc(time.Second*4, func() {
						oldConn.Close() // Close old connection
					})
				}
				p.Debug("close old conn", zap.Any("oldConn", oldConn))
			}
		} else if devceLevel == okproto.DeviceLevelSlave { // 如果设备是slave级别，则把相同的deviceID踢掉
			for _, oldConn := range oldConns {
				if oldConn.DeviceID() == connectPacket.DeviceID {
					p.s.connManager.RemoveConnWithID(oldConn.ID())
					p.s.timingWheel.AfterFunc(time.Second*5, func() {
						oldConn.Close()
					})
				}
			}
		}

	}

	// -------------------- set conn info --------------------
	timeDiff := time.Now().UnixNano()/1000/1000 - connectPacket.ClientTimestamp

	// connCtx := p.connContextPool.Get().(*connContext)
	connCtx := newConnContext(p.s)
	connCtx.init()
	connCtx.conn = conn
	conn.SetContext(connCtx)
	conn.SetProtoVersion(int(connectPacket.Version))
	conn.SetAuthed(true)
	conn.SetDeviceFlag(connectPacket.DeviceFlag.ToUint8())
	conn.SetDeviceID(connectPacket.DeviceID)
	conn.SetUID(connectPacket.UID)
	conn.SetValue(aesKeyKey, aesKey)
	conn.SetValue(aesIVKey, aesIV)
	conn.SetDeviceLevel(devceLevelI)
	conn.SetMaxIdle(p.s.opts.ConnIdleTime)

	p.s.connManager.AddConn(conn)

	// -------------------- response connack --------------------

	lastVersion := connectPacket.Version
	hasServerVersion := false
	if connectPacket.Version > okproto.LatestVersion {
		lastVersion = okproto.LatestVersion
	}
	if connectPacket.Version > 3 {
		hasServerVersion = true
	}

	p.s.Debug("Auth Success", zap.Any("conn", conn), zap.Uint8("protoVersion", connectPacket.Version), zap.Bool("hasServerVersion", hasServerVersion))
	connack := &okproto.ConnackPacket{
		Salt:          aesIV,
		ServerKey:     dhServerPublicKeyEnc,
		ReasonCode:    okproto.ReasonSuccess,
		TimeDiff:      timeDiff,
		ServerVersion: lastVersion,
	}
	connack.HasServerVersion = hasServerVersion
	p.response(conn, connack)

	// -------------------- user online --------------------
	// 在线webhook
	onlineCount, totalOnlineCount := p.s.connManager.GetConnCountWith(uid, connectPacket.DeviceFlag)
	p.s.webhook.Online(uid, connectPacket.DeviceFlag, conn.ID(), onlineCount, totalOnlineCount)

}

// #################### ping ####################
func (p *Processor) processPing(conn oknet.Conn, pingPacket *okproto.PingPacket) {
	p.Debug("ping", zap.Any("conn", conn))
	p.response(conn, &okproto.PongPacket{})
}

// #################### messages ####################
func (p *Processor) processMsgs(conn oknet.Conn, sendPackets []*okproto.SendPacket) {

	var (
		sendackPackets       = make([]okproto.Frame, 0, len(sendPackets)) // response sendack packets
		channelSendPacketMap = make(map[string][]*okproto.SendPacket, 0)  // split sendPacket by channel
		// recvPackets          = make([]okproto.RecvPacket, 0, len(sendpPackets)) // recv packets
	)

	// ########## split sendPacket by channel ##########
	for _, sendPacket := range sendPackets {
		channelKey := fmt.Sprintf("%s-%d", sendPacket.ChannelID, sendPacket.ChannelType)
		channelSendpackets := channelSendPacketMap[channelKey]
		if channelSendpackets == nil {
			channelSendpackets = make([]*okproto.SendPacket, 0, len(sendackPackets))
		}
		channelSendpackets = append(channelSendpackets, sendPacket)
		channelSendPacketMap[channelKey] = channelSendpackets
	}

	// ########## process message for channel ##########
	for _, sendPackets := range channelSendPacketMap {
		firstSendPacket := sendPackets[0]
		channelSendackPackets, err := p.prcocessChannelMessages(conn, firstSendPacket.ChannelID, firstSendPacket.ChannelType, sendPackets)
		if err != nil {
			p.Error("process channel messages err", zap.Error(err))
		}
		if len(channelSendackPackets) > 0 {
			sendackPackets = append(sendackPackets, channelSendackPackets...)
		}
	}
	p.response(conn, sendackPackets...)
}

func (p *Processor) prcocessChannelMessages(conn oknet.Conn, channelID string, channelType uint8, sendPackets []*okproto.SendPacket) ([]okproto.Frame, error) {
	var (
		sendackPackets        = make([]okproto.Frame, 0, len(sendPackets)) // response sendack packets
		messages              = make([]*Message, 0, len(sendPackets))      // recv packets
		err                   error
		respSendackPacketsFnc = func(sendPackets []*okproto.SendPacket, reasonCode okproto.ReasonCode) []okproto.Frame {
			for _, sendPacket := range sendPackets {
				sendackPackets = append(sendackPackets, p.getSendackPacketWithSendPacket(sendPacket, reasonCode))
			}
			return sendackPackets
		}
		respSendackPacketsWithRecvFnc = func(messages []*Message, reasonCode okproto.ReasonCode) []okproto.Frame {
			for _, m := range messages {
				sendackPackets = append(sendackPackets, p.getSendackPacket(m, reasonCode))
			}
			return sendackPackets
		}
	)

	//########## get channel and assert permission ##########
	fakeChannelID := channelID
	if channelType == okproto.ChannelTypePerson {
		fakeChannelID = GetFakeChannelIDWith(conn.UID(), channelID)
	}
	channel, err := p.s.channelManager.GetChannel(fakeChannelID, channelType)
	if err != nil {
		p.Error("getChannel is error", zap.Error(err), zap.String("fakeChannelID", fakeChannelID), zap.Uint8("channelType", channelType))
		return respSendackPacketsFnc(sendPackets, okproto.ReasonSystemError), nil
	}
	if channel == nil {
		p.Error("the channel does not exist or has been disbanded", zap.String("channel_id", fakeChannelID), zap.Uint8("channel_type", channelType))
		return respSendackPacketsFnc(sendPackets, okproto.ReasonChannelNotExist), nil
	}
	hasPerm, reasonCode := p.hasPermission(channel, conn.UID())
	if !hasPerm {
		return respSendackPacketsFnc(sendPackets, reasonCode), nil
	}

	// ########## message decrypt and message store ##########
	for _, sendPacket := range sendPackets {
		var messageID = p.genMessageID() // generate messageID

		if sendPacket.SyncOnce { // client not support send syncOnce message
			sendackPackets = append(sendackPackets, &okproto.SendackPacket{
				Framer:      sendPacket.Framer,
				ClientSeq:   sendPacket.ClientSeq,
				ClientMsgNo: sendPacket.ClientMsgNo,
				MessageID:   messageID,
				ReasonCode:  okproto.ReasonNotSupportHeader,
			})
			continue
		}
		decodePayload, err := p.checkAndDecodePayload(messageID, sendPacket, conn)
		if err != nil {
			p.Error("decode payload err", zap.Error(err))
			p.response(conn, &okproto.SendackPacket{
				Framer:      sendPacket.Framer,
				ClientSeq:   sendPacket.ClientSeq,
				ClientMsgNo: sendPacket.ClientMsgNo,
				MessageID:   messageID,
				ReasonCode:  okproto.ReasonPayloadDecodeError,
			})
			continue
		}

		messages = append(messages, &Message{
			RecvPacket: &okproto.RecvPacket{
				Framer: okproto.Framer{
					RedDot:    sendPacket.GetRedDot(),
					SyncOnce:  sendPacket.GetsyncOnce(),
					NoPersist: sendPacket.GetNoPersist(),
				},
				Setting:     sendPacket.Setting,
				MessageID:   messageID,
				ClientMsgNo: sendPacket.ClientMsgNo,
				StreamNo:    sendPacket.StreamNo,
				StreamFlag:  okproto.StreamFlagIng,
				FromUID:     conn.UID(),
				Expire:      sendPacket.Expire,
				ChannelID:   sendPacket.ChannelID,
				ChannelType: sendPacket.ChannelType,
				Topic:       sendPacket.Topic,
				Timestamp:   int32(time.Now().Unix()),
				Payload:     decodePayload,
				// ---------- 以下不参与编码 ------------
				ClientSeq: sendPacket.ClientSeq,
			},
			fromDeviceFlag: okproto.DeviceFlag(conn.DeviceFlag()),
			fromDeviceID:   conn.DeviceID(),
			large:          channel.Large,
		})
	}
	if len(messages) == 0 {
		return sendackPackets, nil
	}
	err = p.storeChannelMessagesIfNeed(conn.UID(), messages) // only have messageSeq after message save
	if err != nil {
		p.Error("store channel messages err", zap.Error(err))
		return respSendackPacketsWithRecvFnc(messages, okproto.ReasonSystemError), err
	}
	//########## message store to queue ##########
	if p.s.opts.WebhookOn() {
		err = p.storeChannelMessagesToNotifyQueue(messages)
		if err != nil {
			p.Error("store channel messages to notify queue err", zap.Error(err))
			return respSendackPacketsWithRecvFnc(messages, okproto.ReasonSystemError), err
		}
	}

	//########## message put to channel ##########
	err = channel.Put(messages, nil, conn.UID(), okproto.DeviceFlag(conn.DeviceFlag()), conn.DeviceID())
	if err != nil {
		p.Error("put message to channel err", zap.Error(err))
		return respSendackPacketsWithRecvFnc(messages, okproto.ReasonSystemError), err
	}

	//########## respose ##########
	if len(messages) > 0 {
		for _, message := range messages {
			sendackPackets = append(sendackPackets, p.getSendackPacket(message, okproto.ReasonSuccess))
		}

	}
	return sendackPackets, nil
}

// if has permission for sender
func (p *Processor) hasPermission(channel *Channel, fromUID string) (bool, okproto.ReasonCode) {
	if channel.ChannelType == okproto.ChannelTypeCustomerService { // customer service channel
		return true, okproto.ReasonSuccess
	}
	allow, reason := channel.Allow(fromUID)
	if !allow {
		p.Error("The user is not in the white list or in the black list", zap.String("fromUID", fromUID), zap.String("reason", reason.String()))
		return false, reason
	}
	if channel.ChannelType != okproto.ChannelTypePerson && channel.ChannelType != okproto.ChannelTypeInfo {
		if !channel.IsSubscriber(fromUID) && !channel.IsTmpSubscriber(fromUID) {
			p.Error("The user is not in the channel and cannot send messages to the channel", zap.String("fromUID", fromUID), zap.String("channel_id", channel.ChannelID), zap.Uint8("channel_type", channel.ChannelType))
			return false, okproto.ReasonSubscriberNotExist
		}
	}
	return true, okproto.ReasonSuccess
}

func (p *Processor) getSendackPacket(msg *Message, reasonCode okproto.ReasonCode) *okproto.SendackPacket {
	return &okproto.SendackPacket{
		Framer:      msg.Framer,
		ClientMsgNo: msg.ClientMsgNo,
		ClientSeq:   msg.ClientSeq,
		MessageID:   msg.MessageID,
		MessageSeq:  msg.MessageSeq,
		ReasonCode:  reasonCode,
	}

}

func (p *Processor) getSendackPacketWithSendPacket(sendPacket *okproto.SendPacket, reasonCode okproto.ReasonCode) *okproto.SendackPacket {
	return &okproto.SendackPacket{
		Framer:      sendPacket.Framer,
		ClientMsgNo: sendPacket.ClientMsgNo,
		ClientSeq:   sendPacket.ClientSeq,
		ReasonCode:  reasonCode,
	}

}

// store channel messages
func (p *Processor) storeChannelMessagesIfNeed(fromUID string, messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	storeMessages := make([]okstore.Message, 0, len(messages))
	for _, m := range messages {
		if m.NoPersist || m.SyncOnce {
			continue
		}
		if m.StreamIng() { // 流消息单独存储
			_, err := p.s.store.AppendStreamItem(GetFakeChannelIDWith(fromUID, m.ChannelID), m.ChannelType, m.StreamNo, &okstore.StreamItem{
				ClientMsgNo: m.ClientMsgNo,
				Blob:        m.Payload,
			})
			if err != nil {
				p.Error("store stream item err", zap.Error(err))
				return err
			}
			continue
		}
		storeMessages = append(storeMessages, m)
	}
	if len(storeMessages) == 0 {
		return nil
	}
	firstMessage := storeMessages[0].(*Message)
	fakeChannelID := firstMessage.ChannelID
	if firstMessage.ChannelType == okproto.ChannelTypePerson {
		fakeChannelID = GetFakeChannelIDWith(fromUID, firstMessage.ChannelID)
	}
	_, err := p.s.store.AppendMessages(fakeChannelID, firstMessage.ChannelType, storeMessages)
	if err != nil {
		p.Error("store message err", zap.Error(err))
		return err
	}
	return nil
}

func (p *Processor) storeChannelMessagesToNotifyQueue(messages []*Message) error {
	if len(messages) == 0 {
		return nil
	}
	storeMessages := make([]okstore.Message, 0, len(messages))
	for _, m := range messages {
		if m.StreamIng() || (m.NoPersist && m.SyncOnce) { // 流消息不做通知（只通知开始和结束）,不存储的消息也不通知
			continue
		}
		storeMessages = append(storeMessages, m)
	}
	return p.s.store.AppendMessageOfNotifyQueue(storeMessages)
}

// decode payload
func (p *Processor) checkAndDecodePayload(messageID int64, sendPacket *okproto.SendPacket, c oknet.Conn) ([]byte, error) {
	var (
		aesKey = c.Value(aesKeyKey).(string)
		aesIV  = c.Value(aesIVKey).(string)
	)
	vail, err := p.sendPacketIsVail(sendPacket, c)
	if err != nil {
		return nil, err
	}
	if !vail {
		return nil, errors.New("sendPacket is illegal！")
	}
	// decode payload
	decodePayload, err := okutil.AesDecryptPkcs7Base64(sendPacket.Payload, []byte(aesKey), []byte(aesIV))
	if err != nil {
		p.Error("Failed to decode payload！", zap.Error(err))
		return nil, err
	}

	return decodePayload, nil
}

// send packet is vail
func (p *Processor) sendPacketIsVail(sendPacket *okproto.SendPacket, c oknet.Conn) (bool, error) {
	var (
		aesKey = c.Value(aesKeyKey).(string)
		aesIV  = c.Value(aesIVKey).(string)
	)
	signStr := sendPacket.VerityString()
	actMsgKey, err := okutil.AesEncryptPkcs7Base64([]byte(signStr), []byte(aesKey), []byte(aesIV))
	if err != nil {
		p.Error("msgKey is illegal！", zap.Error(err), zap.String("sign", signStr), zap.String("aesKey", aesKey), zap.String("aesIV", aesIV), zap.Any("conn", c))
		return false, err
	}
	actMsgKeyStr := sendPacket.MsgKey
	exceptMsgKey := okutil.MD5(string(actMsgKey))
	if actMsgKeyStr != exceptMsgKey {
		p.Error("msgKey is illegal！", zap.String("except", exceptMsgKey), zap.String("act", actMsgKeyStr), zap.String("sign", signStr), zap.String("aesKey", aesKey), zap.String("aesIV", aesIV), zap.Any("conn", c))
		return false, errors.New("msgKey is illegal！")
	}
	return true, nil
}

// #################### subscribe ####################
func (p *Processor) processSubs(conn oknet.Conn, subPackets []*okproto.SubPacket) {
	for _, subPacket := range subPackets {
		p.processSub(conn, subPacket)
	}
}

func (p *Processor) processSub(conn oknet.Conn, subPacket *okproto.SubPacket) {

	channelIDUrl, err := url.Parse(subPacket.ChannelID)
	if err != nil {
		p.Warn("订阅的频道ID不合法！", zap.Error(err), zap.String("channelID", subPacket.ChannelID))
		return
	}
	channelID := channelIDUrl.Path
	if strings.TrimSpace(channelID) == "" {
		p.Warn("订阅的频道ID不能为空！", zap.String("channelID", subPacket.ChannelID))
		return
	}

	paramMap := map[string]interface{}{}

	values := channelIDUrl.Query()
	for key, value := range values {
		if len(value) > 0 {
			paramMap[key] = value[0]
		}
	}

	if subPacket.ChannelType != okproto.ChannelTypeData {
		p.Warn("订阅的频道类型不正确！", zap.Uint8("channelType", subPacket.ChannelType))
		p.response(conn, p.getSuback(subPacket, channelID, okproto.ReasonNotSupportChannelType))
		return
	}

	channel, err := p.s.channelManager.GetChannel(channelID, subPacket.ChannelType)
	if err != nil {
		p.Warn("获取频道失败！", zap.Error(err))
		p.response(conn, p.getSuback(subPacket, channelID, okproto.ReasonSystemError))
		return
	}
	if channel == nil {
		p.Warn("频道不存在！", zap.String("channelID", channelID), zap.Uint8("channelType", subPacket.ChannelType))
		p.response(conn, p.getSuback(subPacket, channelID, okproto.ReasonChannelNotExist))
		return
	}

	connCtx := conn.Context().(*connContext)
	if subPacket.Action == okproto.Subscribe {
		if strings.TrimSpace(subPacket.Param) != "" {
			paramM, _ := okutil.JSONToMap(subPacket.Param)
			if len(paramM) > 0 {
				for k, v := range paramM {
					paramMap[k] = v
				}
			}
		}
		channel.AddSubscriber(conn.UID())
		connCtx.subscribeChannel(channelID, subPacket.ChannelType, paramMap)
	} else {

		if !p.s.connManager.ExistConnsWithUID(conn.UID()) {
			channel.RemoveSubscriber(conn.UID())
		}
		if subPacket.ChannelType == okproto.ChannelTypeData && len(channel.GetAllSubscribers()) == 0 {
			p.s.channelManager.RemoveDataChannel(channelID, subPacket.ChannelType)
		}
		connCtx.unscribeChannel(channelID, subPacket.ChannelType)
	}
	p.response(conn, p.getSuback(subPacket, channelID, okproto.ReasonSuccess))
}

func (p *Processor) getSuback(subPacket *okproto.SubPacket, channelID string, reasonCode okproto.ReasonCode) *okproto.SubackPacket {
	return &okproto.SubackPacket{
		SubNo:       subPacket.SubNo,
		ChannelID:   channelID,
		ChannelType: subPacket.ChannelType,
		Action:      subPacket.Action,
		ReasonCode:  reasonCode,
	}
}

// #################### recv ack ####################
func (p *Processor) processRecvacks(conn oknet.Conn, acks []*okproto.RecvackPacket) {
	if len(acks) == 0 {
		return
	}
	for _, ack := range acks {
		persist := !ack.NoPersist
		if persist {
			// 完成消息（移除重试队列里的消息）
			p.Debug("移除重试队列里的消息！", zap.Uint32("messageSeq", ack.MessageSeq), zap.String("uid", conn.UID()), zap.Int64("clientID", conn.ID()), zap.Uint8("deviceFlag", conn.DeviceFlag()), zap.Uint8("deviceLevel", conn.DeviceLevel()), zap.String("deviceID", conn.DeviceID()), zap.Bool("syncOnce", ack.SyncOnce), zap.Bool("noPersist", ack.NoPersist), zap.Int64("messageID", ack.MessageID))
			err := p.s.retryQueue.finishMessage(conn.UID(), conn.DeviceID(), ack.MessageID)
			if err != nil {
				p.Warn("移除重试队列里的消息失败！", zap.Error(err), zap.Uint32("messageSeq", ack.MessageSeq), zap.String("uid", conn.UID()), zap.Int64("clientID", conn.ID()), zap.Uint8("deviceFlag", conn.DeviceFlag()), zap.String("deviceID", conn.DeviceID()), zap.Int64("messageID", ack.MessageID))
			}
		}
		if ack.SyncOnce && persist && okproto.DeviceLevel(conn.DeviceLevel()) == okproto.DeviceLevelMaster { // 写扩散和存储并且是master等级的设备才会更新游标
			p.Debug("更新游标", zap.String("uid", conn.UID()), zap.Uint32("messageSeq", ack.MessageSeq))
			err := p.s.store.UpdateMessageOfUserCursorIfNeed(conn.UID(), ack.MessageSeq)
			if err != nil {
				p.Warn("更新游标失败！", zap.Error(err), zap.String("uid", conn.UID()), zap.Uint32("messageSeq", ack.MessageSeq))
			}
		}
	}

}

// #################### process conn close ####################
func (p *Processor) processClose(conn oknet.Conn) {
	p.Debug("conn is close", zap.Any("conn", conn))
	if conn.Context() != nil {
		p.s.connManager.RemoveConn(conn)
		connCtx := conn.Context().(*connContext)
		connCtx.release()
		p.connContextPool.Put(connCtx)

		onlineCount, totalOnlineCount := p.s.connManager.GetConnCountWith(conn.UID(), okproto.DeviceFlag(conn.DeviceFlag())) // 指定的uid和设备下没有新的客户端才算真真的下线（TODO: 有时候离线要比在线晚触发导致不正确）
		p.s.webhook.Offline(conn.UID(), okproto.DeviceFlag(conn.DeviceFlag()), conn.ID(), onlineCount, totalOnlineCount)     // 触发离线webhook
	}
}

// #################### others ####################

func (p *Processor) response(conn oknet.Conn, frames ...okproto.Frame) {
	p.s.dispatch.dataOut(conn, frames...)
}

func (p *Processor) responseConnackAuthFail(c oknet.Conn) {
	p.responseConnack(c, 0, okproto.ReasonAuthFail)
}

func (p *Processor) responseConnack(c oknet.Conn, timeDiff int64, code okproto.ReasonCode) {

	p.response(c, &okproto.ConnackPacket{
		ReasonCode: code,
		TimeDiff:   timeDiff,
	})
}

// 获取客户端的aesKey和aesIV
// dhServerPrivKey  服务端私钥
func (p *Processor) getClientAesKeyAndIV(clientKey string, dhServerPrivKey [32]byte) (string, string, error) {

	clientKeyBytes, err := base64.StdEncoding.DecodeString(clientKey)
	if err != nil {
		return "", "", err
	}

	var dhClientPubKeyArray [32]byte
	copy(dhClientPubKeyArray[:], clientKeyBytes[:32])

	// 获得DH的共享key
	shareKey := okutil.GetCurve25519Key(dhServerPrivKey, dhClientPubKeyArray) // 共享key

	aesIV := okutil.GetRandomString(16)
	aesKey := okutil.MD5(base64.StdEncoding.EncodeToString(shareKey[:]))[:16]
	return aesKey, aesIV, nil
}

// 生成消息ID
func (p *Processor) genMessageID() int64 {
	return p.messageIDGen.Generate().Int64()
}

func (p *Processor) process(conn oknet.Conn) {
	connCtx := conn.Context().(*connContext)
	frames := connCtx.popFrames()
	p.processFrames(conn, frames)

}

// 处理相同的frame
func (p *Processor) processFrames(conn oknet.Conn, frames []okproto.Frame) {

	p.sameFrames(frames, func(s, e int, frs []okproto.Frame) {

		p.frameWorkPool.Submit(func() { // 开启协程处理相同的frame
			p.processSameFrame(conn, frs[0].GetFrameType(), frs, s, e)
		})
	})

}

// 将frames按照frameType分组，然后处理
func (p *Processor) sameFrames(frames []okproto.Frame, callback func(s, e int, fs []okproto.Frame)) {
	for i := 0; i < len(frames); {
		frame := frames[i]
		start := i
		end := i + 1
		for end < len(frames) {
			nextFrame := frames[end]
			if nextFrame.GetFrameType() == frame.GetFrameType() {
				end++
			} else {
				break
			}
		}
		callback(start, end, frames[start:end])
		i = end
	}
}
