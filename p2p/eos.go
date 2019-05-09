package p2p

import (
	"encoding/hex"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	eos "github.com/eosforce/goeosforce"
	"github.com/eosforce/goeosforce/p2p"
	"github.com/fanyang1988/force-go/types"
	"go.uber.org/zap"
)

type Envelope4EOS struct {
	Peer    string     `json:"peer"`
	Packet  eos.Packet `json:"packet"`
	IsClose bool
}

// p2pEOSClient a manager for peers to diff p2p node
type p2pEOSClient struct {
	name      string
	clients   []*p2p.Client
	handlers  []types.P2PHandler
	msgChan   chan Envelope4EOS
	wg        sync.WaitGroup
	chanWg    sync.WaitGroup
	switcher  types.SwitcherInterface
	hasClosed bool
	mutex     sync.RWMutex
	logger    *zap.Logger
}

func (p *p2pEOSClient) Type() types.ClientType {
	return types.EOSIO
}

// NewP2PPeers new p2p peers from cfg
func NewP2PClient4EOS(name string, chainID string, startBlock *P2PSyncData, peers []string, logger *zap.Logger) *p2pEOSClient {
	p := &p2pEOSClient{
		name:     name,
		clients:  make([]*p2p.Client, 0, len(peers)),
		handlers: make([]types.P2PHandler, 0, 8),
		msgChan:  make(chan Envelope4EOS, 64),
		logger:   logger,
	}

	p.switcher = types.NewSwitcherInterface(p.Type())

	cID, err := hex.DecodeString(chainID)
	if err != nil {
		p.logger.Error("decode chain id err", zap.Error(err))
		panic(err)
	}

	var startBlockNum uint32 = 1
	var startBlockId eos.Checksum256
	var startBlockTime time.Time
	var irrBlockNum uint32 = 0
	var irrBlockId eos.Checksum256
	if startBlock != nil {
		startBlockId = eos.Checksum256(startBlock.HeadBlockID)
		startBlockNum = startBlock.HeadBlockNum
		startBlockTime = startBlock.HeadBlockTime
		irrBlockNum = startBlock.LastIrreversibleBlockNum
		irrBlockId = eos.Checksum256(startBlock.LastIrreversibleBlockID)
	}
	for idx, peer := range peers {
		p.logger.Debug("new peer client", zap.Int("idx", idx), zap.String("peer", peer))
		client := p2p.NewClient(
			p2p.NewOutgoingPeer(peer, fmt.Sprintf("%s-%02d", name, idx), &p2p.HandshakeInfo{
				ChainID:                  cID,
				HeadBlockNum:             startBlockNum,
				HeadBlockID:              startBlockId,
				HeadBlockTime:            startBlockTime,
				LastIrreversibleBlockNum: irrBlockNum,
				LastIrreversibleBlockID:  irrBlockId,
			}),
			true,
		)
		client.RegisterHandler(p)
		p.clients = append(p.clients, client)
	}

	return p
}

func (p *p2pEOSClient) Start() error {
	p.chanWg.Add(1)
	go func() {
		defer p.chanWg.Done()
		for {
			isStop := p.Loop()
			if isStop {
				p.logger.Info("p2p peers stop")
				return
			}
		}
	}()

	for idx, client := range p.clients {
		p.createClient(idx, client)
	}

	return nil
}

func (p *p2pEOSClient) IsClosed() bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.hasClosed
}

func (p *p2pEOSClient) createClient(idx int, client *p2p.Client) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			p.logger.Info("create connect", zap.Int("client", idx))
			err := client.Start()

			// check when after close client
			if p.IsClosed() {
				return
			}

			if err != nil {
				p.logger.Error("client err", zap.Int("client", idx), zap.Error(err))
			}

			time.Sleep(3 * time.Second)

			// check when after sleep
			if p.IsClosed() {
				return
			}
		}
	}()
}

func (p *p2pEOSClient) CloseConnection() error {
	p.logger.Warn("start close")

	p.mutex.Lock()
	p.hasClosed = true
	p.mutex.Unlock()

	for idx, client := range p.clients {
		go func(i int, cli *p2p.Client) {
			err := cli.CloseConnection()
			if err != nil {
				p.logger.Error("client close err", zap.Int("client", i), zap.Error(err))
			}
			p.logger.Info("client close", zap.Int("client", i))
		}(idx, client)
	}
	p.wg.Wait()
	p.msgChan <- Envelope4EOS{
		IsClose: true,
	}
	close(p.msgChan)
	p.chanWg.Wait()

	return nil
}

func (p *p2pEOSClient) Loop() bool {
	ev, ok := <-p.msgChan
	if ev.IsClose {
		return true
	}

	if !ok {
		p.logger.Warn("p2p peers msg chan closed")
		return true
	}

	p.handleImp(&ev)

	return false
}

func (p *p2pEOSClient) handleImp(envelope *Envelope4EOS) {
	for _, h := range p.handlers {
		func(hh types.P2PHandler) {
			defer func() {
				if err := recover(); err != nil {
					p.logger.Error("handler process ev panic",
						zap.String("err", fmt.Sprintf("err:%s", err)),
						zap.String("stack", string(debug.Stack())))
				}
			}()

			var err error
			switch envelope.Packet.Type {
			case eos.GoAwayMessageType:
				m, ok := envelope.Packet.P2PMessage.(*eos.GoAwayMessage)
				if !ok {
					p.logger.Error("msg type err by go away")
					return
				}
				p.logger.Info("peer goaway",
					zap.String("peer", envelope.Peer),
					zap.String("reason", m.Reason.String()),
					zap.String("nodeid", m.NodeID.String()))
				err = hh.OnGoAway(envelope.Peer, uint8(m.Reason), types.Checksum256(m.NodeID))
			case eos.SignedBlockType:
				m, ok := envelope.Packet.P2PMessage.(*eos.SignedBlock)
				if !ok {
					p.logger.Error("msg type err by go away")
					return
				}
				p.logger.Debug("on signed block",
					zap.String("peer", envelope.Peer),
					zap.String("block", m.String()))
				msg, err := p.switcher.BlockToCommon(m)
				if err == nil {
					err = hh.OnBlock(envelope.Peer, msg)
				} else {
					p.logger.Error("handle msg err", zap.Error(err))
				}

			}

			if err != nil {
				p.logger.Error("handle msg err", zap.Error(err))
			}

		}(h)
	}
}

// Handle handler for p2p clients
func (p *p2pEOSClient) Handle(envelope *p2p.Envelope) {
	p.msgChan <- Envelope4EOS{
		Peer:   envelope.Sender.Address,
		Packet: *envelope.Packet,
	}
}

func (p *p2pEOSClient) RegHandler(handler types.P2PHandler) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	p.handlers = append(p.handlers, handler)
}

func (p *p2pEOSClient) SetReadTimeout(readTimeout time.Duration) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	for _, peer := range p.clients {
		peer.SetReadTimeout(readTimeout)
	}
}
