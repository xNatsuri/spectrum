package session

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cooldogedev/spectrum/internal"
	"github.com/cooldogedev/spectrum/server"
	"github.com/cooldogedev/spectrum/session/animation"
	"github.com/cooldogedev/spectrum/transport"
	"github.com/cooldogedev/spectrum/util"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

type Session struct {
	clientConn *minecraft.Conn

	serverAddr string
	serverConn *server.Conn
	serverMu   sync.RWMutex

	logger   internal.Logger
	registry *Registry

	discovery server.Discovery
	opts      util.Opts
	transport transport.Transport

	animation animation.Animation
	processor Processor
	tracker   *Tracker

	closed       chan struct{}
	latency      int64
	transferring atomic.Bool
}

func NewSession(clientConn *minecraft.Conn, logger internal.Logger, registry *Registry, discovery server.Discovery, opts util.Opts, transport transport.Transport) *Session {
	s := &Session{
		clientConn: clientConn,

		logger:   logger,
		registry: registry,

		discovery: discovery,
		opts:      opts,
		transport: transport,

		animation: &animation.Dimension{},
		tracker:   NewTracker(),

		closed:  make(chan struct{}),
		latency: 0,
	}
	s.serverMu.Lock()
	return s
}

func (s *Session) Login() (err error) {
	defer s.serverMu.Unlock()

	serverAddr, err := s.discovery.Discover(s.clientConn)
	if err != nil {
		s.logger.Debugf("Failed to discover a server: %v", err)
		return err
	}

	serverConn, err := s.dial(serverAddr)
	if err != nil {
		s.logger.Errorf("Failed to dial server: %v", err)
		return err
	}

	s.serverAddr = serverAddr
	s.serverConn = serverConn
	if err := serverConn.Connect(s.clientConn, s.opts.Token); err != nil {
		s.logger.Errorf("Failed to start connection sequence: %v", err)
		return err
	}

	if err := serverConn.Spawn(); err != nil {
		s.logger.Errorf("Failed to start spawn sequence: %v", err)
		return err
	}

	if err := s.clientConn.StartGame(serverConn.GameData()); err != nil {
		s.logger.Errorf("Failed to start game timeout: %v", err)
		return err
	}

	s.sendMetadata(true)

	go handleIncoming(s)
	go handleOutgoing(s)
	go handleLatency(s, s.opts.LatencyInterval)

	identityData := s.clientConn.IdentityData()
	s.registry.AddSession(identityData.XUID, s)
	s.logger.Infof("Successfully logged in %s", identityData.DisplayName)
	return
}

func (s *Session) Transfer(addr string) error {
	if !s.transferring.CompareAndSwap(false, true) {
		return errors.New("already transferring")
	}

	s.serverMu.Lock()
	defer func() {
		s.serverMu.Unlock()
		s.transferring.Store(false)
	}()

	if s.serverAddr == addr {
		return errors.New("already connected to this server")
	}

	if s.processor != nil && !s.processor.ProcessPreTransfer(addr) {
		return errors.New("processor failed")
	}

	conn, err := s.dial(addr)
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return fmt.Errorf("failed to dial server: %v", err)
	}

	s.sendMetadata(true)
	if err := conn.Connect(s.clientConn, s.opts.Token); err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		s.sendMetadata(false)
		return fmt.Errorf("failed to start connection sequence: %v", err)
	}

	if err := conn.Spawn(); err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		s.sendMetadata(false)
		return fmt.Errorf("failed to start spawn sequence: %v", err)
	}

	serverGameData := conn.GameData()
	s.animation.Play(s.clientConn, serverGameData)

	chunk := emptyChunk(serverGameData.Dimension)
	pos := serverGameData.PlayerPosition
	chunkX := int32(pos.X()) >> 4
	chunkZ := int32(pos.Z()) >> 4
	for x := chunkX - 4; x <= chunkX+4; x++ {
		for z := chunkZ - 4; z <= chunkZ+4; z++ {
			_ = s.clientConn.WritePacket(&packet.LevelChunk{
				Dimension:     packet.DimensionNether,
				Position:      protocol.ChunkPos{x, z},
				SubChunkCount: 1,
				RawPayload:    chunk,
			})
		}
	}

	s.tracker.clearEffects(s)
	s.tracker.clearEntities(s)
	s.tracker.clearBossBars(s)
	s.tracker.clearPlayers(s)
	s.tracker.clearScoreboards(s)

	_ = s.clientConn.WritePacket(&packet.MovePlayer{
		EntityRuntimeID: serverGameData.EntityRuntimeID,
		Position:        serverGameData.PlayerPosition,
		Pitch:           serverGameData.Pitch,
		Yaw:             serverGameData.Yaw,
		Mode:            packet.MoveModeReset,
	})

	_ = s.clientConn.WritePacket(&packet.LevelEvent{
		EventType: packet.LevelEventStopRaining,
		EventData: 10_000,
	})
	_ = s.clientConn.WritePacket(&packet.LevelEvent{
		EventType: packet.LevelEventStopThunderstorm,
	})

	_ = s.clientConn.WritePacket(&packet.SetDifficulty{
		Difficulty: uint32(serverGameData.Difficulty),
	})
	_ = s.clientConn.WritePacket(&packet.SetPlayerGameType{
		GameType: serverGameData.PlayerGameMode,
	})

	_ = s.clientConn.WritePacket(&packet.GameRulesChanged{
		GameRules: serverGameData.GameRules,
	})

	s.animation.Clear(s.clientConn, serverGameData)
	_ = s.serverConn.Close()
	s.serverAddr = addr
	s.serverConn = conn

	if s.processor != nil {
		s.processor.ProcessPostTransfer(addr)
	}
	s.logger.Debugf("Transferred session for %s to %s", s.clientConn.IdentityData().DisplayName, addr)
	return nil
}

func (s *Session) Animation() animation.Animation {
	return s.animation
}

func (s *Session) SetAnimation(animation animation.Animation) {
	s.animation = animation
}

func (s *Session) Opts() util.Opts {
	return s.opts
}

func (s *Session) SetOpts(opts util.Opts) {
	s.opts = opts
}

func (s *Session) Processor() Processor {
	return s.processor
}

func (s *Session) SetProcessor(processor Processor) {
	s.processor = processor
}

func (s *Session) Latency() int64 {
	return s.clientConn.Latency().Milliseconds() + s.latency
}

func (s *Session) Client() *minecraft.Conn {
	return s.clientConn
}

func (s *Session) Server() *server.Conn {
	s.serverMu.RLock()
	defer s.serverMu.RUnlock()
	return s.serverConn
}

func (s *Session) Disconnect(message string) {
	_ = s.clientConn.WritePacket(&packet.Disconnect{Message: message})
	_ = s.Close()
}

func (s *Session) Close() (err error) {
	select {
	case <-s.closed:
		return errors.New("already closed")
	default:
		close(s.closed)

		if s.processor != nil {
			s.processor.ProcessDisconnection()
			s.processor = nil
		}

		_ = s.clientConn.Close()
		if s.serverConn != nil {
			_ = s.serverConn.Close()
			s.serverConn = nil
		}

		identity := s.clientConn.IdentityData()
		s.registry.RemoveSession(identity.XUID)
		s.logger.Infof("Closed session for %s", identity.DisplayName)
		return
	}
}

func (s *Session) dial(addr string) (*server.Conn, error) {
	conn, err := s.transport.Dial(addr)
	if err != nil {
		return nil, err
	}
	return server.NewConn(conn, packet.NewServerPool()), nil
}

func (s *Session) sendMetadata(noAI bool) {
	metadata := protocol.NewEntityMetadata()
	if noAI {
		metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagNoAI)
	}
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagBreathing)
	metadata.SetFlag(protocol.EntityDataKeyFlags, protocol.EntityDataFlagHasGravity)
	_ = s.clientConn.WritePacket(&packet.SetActorData{
		EntityRuntimeID: s.clientConn.GameData().EntityRuntimeID,
		EntityMetadata:  metadata,
	})
}
