package network

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/CityOfZion/neo-go/pkg/core"
	"github.com/CityOfZion/neo-go/pkg/network/payload"
	"github.com/CityOfZion/neo-go/pkg/util"
	log "github.com/sirupsen/logrus"
)

const (
	minPeers      = 5
	maxBlockBatch = 200
	minPoolCount  = 30
)

var (
	errPortMismatch     = errors.New("port mismatch")
	errIdenticalID      = errors.New("identical node id")
	errInvalidHandshake = errors.New("invalid handshake")
	errInvalidNetwork   = errors.New("invalid network")
	errServerShutdown   = errors.New("server shutdown")
	errInvalidInvType   = errors.New("invalid inventory type")
)

type (
	// Server represents the local Node in the network. Its transport could
	// be of any kind.
	Server struct {
		// ServerConfig holds the Server configuration.
		ServerConfig

		// id also known as the nonce of the server.
		id uint32

		transport Transporter
		discovery Discoverer
		chain     core.Blockchainer

		lock  sync.RWMutex
		peers map[Peer]bool

		register   chan Peer
		unregister chan peerDrop
		quit       chan struct{}

		proto <-chan protoTuple
	}

	protoTuple struct {
		msg  *Message
		peer Peer
	}

	peerDrop struct {
		peer   Peer
		reason error
	}
)

// NewServer returns a new Server, initialized with the given configuration.
func NewServer(config ServerConfig, chain *core.Blockchain) *Server {
	s := &Server{
		ServerConfig: config,
		chain:        chain,
		id:           util.RandUint32(1000000, 9999999),
		quit:         make(chan struct{}),
		register:     make(chan Peer),
		unregister:   make(chan peerDrop),
		peers:        make(map[Peer]bool),
	}

	s.transport = NewTCPTransport(s, fmt.Sprintf(":%d", config.ListenTCP))
	s.proto = s.transport.Consumer()
	s.discovery = NewDefaultDiscovery(
		s.DialTimeout,
		s.transport,
	)

	return s
}

// ID returns the servers ID.
func (s *Server) ID() uint32 {
	return s.id
}

// Start will start the server and its underlying transport.
func (s *Server) Start(errChan chan error) {
	log.WithFields(log.Fields{
		"blockHeight":  s.chain.BlockHeight(),
		"headerHeight": s.chain.HeaderHeight(),
	}).Info("node started")

	go s.transport.Accept()
	s.discovery.BackFill(s.Seeds...)
	s.run()
}

// Shutdown disconnects all peers and stops listening.
func (s *Server) Shutdown() {
	log.WithFields(log.Fields{
		"peers": s.PeerCount(),
	}).Info("shutting down server")
	s.discovery.Close()
	close(s.quit)
}

// UnconnectedPeers returns the current count of
// peers the node isn't connected to but has discovered.
func (s *Server) UnconnectedPeers() []string {
	return s.discovery.UnconnectedPeers()
}

// BadPeers returns the current count of peers
// the node has classed as bad.
func (s *Server) BadPeers() []string {
	return s.discovery.BadPeers()
}

func (s *Server) run() {
	// Ask discovery to connect with remote nodes to fill up
	// the server minimum peer slots.
	s.discovery.RequestRemote(minPeers - s.PeerCount())

	for {
		select {
		case proto := <-s.proto:
			if err := s.processProto(proto); err != nil {
				proto.peer.Disconnect(err)
				// verack and version implies that the protocol is
				// not started and the only way to disconnect them
				// from the server is to manually call unregister.
				switch proto.msg.CommandType() {
				case CMDVerack, CMDVersion:
					go func() {
						s.unregister <- peerDrop{proto.peer, err}
					}()
				}
			}
		case <-s.quit:
			s.transport.Close()
			for p := range s.peers {
				p.Disconnect(errServerShutdown)
			}
			return
		case p := <-s.register:
			// When a new peer is connected we send out our version immediately.
			s.sendVersion(p)
			s.peers[p] = true
			log.WithFields(log.Fields{
				"endpoint": p.Endpoint(),
			}).Info("new peer connected")
		case drop := <-s.unregister:
			s.discovery.RegisterBadAddr(drop.peer.Endpoint().String())
			delete(s.peers, drop.peer)
			log.WithFields(log.Fields{
				"endpoint":  drop.peer.Endpoint(),
				"reason":    drop.reason,
				"peerCount": s.PeerCount(),
			}).Warn("peer disconnected")
		}
	}
}

// Peers returns the current list of peers connected to
// the server.
func (s *Server) Peers() map[Peer]bool {
	return s.peers
}

// PeerCount returns the number of current connected peers.
func (s *Server) PeerCount() int {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return len(s.peers)
}

// startProtocol starts a long running background loop that interacts
// every ProtoTickInterval with the peer.
func (s *Server) startProtocol(p Peer) {
	log.WithFields(log.Fields{
		"endpoint":    p.Endpoint(),
		"userAgent":   string(p.Version().UserAgent),
		"startHeight": p.Version().StartHeight,
		"id":          p.Version().Nonce,
	}).Info("started protocol")

	s.requestHeaders(p)
	s.requestPeerInfo(p)

	timer := time.NewTimer(s.ProtoTickInterval)
	for {
		select {
		case err := <-p.Done():
			s.unregister <- peerDrop{p, err}
			return
		case <-timer.C:
			// Try to sync in headers and block with the peer if his block height is higher then ours.
			if p.Version().StartHeight > s.chain.BlockHeight() {
				s.requestBlocks(p)
			}
			// If the discovery does not have a healthy address pool
			// we will ask for a new batch of addresses.
			if s.discovery.PoolCount() < minPoolCount {
				s.requestPeerInfo(p)
			}
			timer.Reset(s.ProtoTickInterval)
		}
	}
}

// When a peer connects to the server, we will send our version immediately.
func (s *Server) sendVersion(p Peer) {
	payload := payload.NewVersion(
		s.id,
		s.ListenTCP,
		s.UserAgent,
		s.chain.BlockHeight(),
		s.Relay,
	)
	p.Send(NewMessage(s.Net, CMDVersion, payload))
}

// When a peer sends out his version we reply with verack after validating
// the version.
func (s *Server) handleVersionCmd(p Peer, version *payload.Version) error {
	if p.Endpoint().Port != version.Port {
		return errPortMismatch
	}
	if s.id == version.Nonce {
		return errIdenticalID
	}
	p.Send(NewMessage(s.Net, CMDVerack, nil))
	return nil
}

// handleHeadersCmd will process the headers it received from its peer.
// if the headerHeight of the blockchain still smaller then the peer
// the server will request more headers.
// This method could best be called in a separate routine.
func (s *Server) handleHeadersCmd(p Peer, headers *payload.Headers) {
	if err := s.chain.AddHeaders(headers.Hdrs...); err != nil {
		log.Warnf("failed processing headers: %s", err)
		return
	}
	// The peer will respond with a maximum of 2000 headers in one batch.
	// We will ask one more batch here if needed. Eventually we will get synced
	// due to the startProtocol routine that will ask headers every protoTick.
	if s.chain.HeaderHeight() < p.Version().StartHeight {
		s.requestHeaders(p)
	}
}

// handleBlockCmd processes the received block received from its peer.
func (s *Server) handleBlockCmd(p Peer, block *core.Block) error {
	if !s.chain.HasBlock(block.Hash()) {
		return s.chain.AddBlock(block)
	}
	return nil
}

// handleInvCmd will process the received inventory.
func (s *Server) handleInvCmd(p Peer, inv *payload.Inventory) error {
	if !inv.Type.Valid() || len(inv.Hashes) == 0 {
		return errInvalidInvType
	}
	payload := payload.NewInventory(inv.Type, inv.Hashes)
	p.Send(NewMessage(s.Net, CMDGetData, payload))
	return nil
}

func (s *Server) handleGetHeadersCmd(p Peer, getHeaders *payload.GetBlocks) error {
	log.Info(getHeaders)
	return nil
}

// requestHeaders will send a getheaders message to the peer.
// The peer will respond with headers op to a count of 2000.
func (s *Server) requestHeaders(p Peer) {
	start := []util.Uint256{s.chain.CurrentHeaderHash()}
	payload := payload.NewGetBlocks(start, util.Uint256{})
	p.Send(NewMessage(s.Net, CMDGetHeaders, payload))
}

// requestPeerInfo will send a getaddr message to the peer
// which will respond with his known addresses in the network.
func (s *Server) requestPeerInfo(p Peer) {
	p.Send(NewMessage(s.Net, CMDGetAddr, nil))
}

// requestBlocks will send a getdata message to the peer
// to sync up in blocks. A maximum of maxBlockBatch will
// send at once.
func (s *Server) requestBlocks(p Peer) {
	var (
		hashStart    = s.chain.BlockHeight() + 1
		headerHeight = s.chain.HeaderHeight()
		hashes       = []util.Uint256{}
	)
	for hashStart < headerHeight && len(hashes) < maxBlockBatch {
		hash := s.chain.GetHeaderHash(int(hashStart))
		hashes = append(hashes, hash)
		hashStart++
	}
	if len(hashes) > 0 {
		payload := payload.NewInventory(payload.BlockType, hashes)
		p.Send(NewMessage(s.Net, CMDGetData, payload))
	} else if s.chain.HeaderHeight() < p.Version().StartHeight {
		s.requestHeaders(p)
	}
}

// process the received protocol message.
func (s *Server) processProto(proto protoTuple) error {
	var (
		peer = proto.peer
		msg  = proto.msg
	)

	// Make sure both server and peer are operating on
	// the same network.
	if msg.Magic != s.Net {
		return errInvalidNetwork
	}

	switch msg.CommandType() {
	case CMDVersion:
		version := msg.Payload.(*payload.Version)
		return s.handleVersionCmd(peer, version)
	case CMDHeaders:
		headers := msg.Payload.(*payload.Headers)
		go s.handleHeadersCmd(peer, headers)
	case CMDInv:
		inventory := msg.Payload.(*payload.Inventory)
		return s.handleInvCmd(peer, inventory)
	case CMDBlock:
		block := msg.Payload.(*core.Block)
		return s.handleBlockCmd(peer, block)
	case CMDGetHeaders:
		getHeaders := msg.Payload.(*payload.GetBlocks)
		s.handleGetHeadersCmd(peer, getHeaders)
	case CMDVerack:
		// Make sure this peer has sended his version before we start the
		// protocol.
		if peer.Version() == nil {
			return errInvalidHandshake
		}
		go s.startProtocol(peer)
	case CMDAddr:
		addressList := msg.Payload.(*payload.AddressList)
		for _, addr := range addressList.Addrs {
			s.discovery.BackFill(addr.Endpoint.String())
		}
	}
	return nil
}
