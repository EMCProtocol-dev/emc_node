package application

import (
	appProto "github.com/emc-protocol/edge-matrix/application/proto"
	"github.com/emc-protocol/edge-matrix/types"
	"github.com/emc-protocol/edge-matrix/validators"
	"github.com/hashicorp/go-hclog"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"time"
)

const (
	appSyncerProto = "/appsyncer/0.1"
	syncerName     = "appsyncer"
)

const (
	DefaultAppStatusPublishDuration = 15 * 60 * time.Second
)

type blockchainStore interface {
	// Header returns the current msg of the chain (genesis if empty)
	Header() *types.Header

	// GetHeaderByNumber gets a msg using the provided number
	GetHeaderByNumber(uint64) (*types.Header, bool)

	// GetBlockByHash gets a block using the provided hash
	GetBlockByHash(hash types.Hash, full bool) (*types.Block, bool)

	// GetBlockByNumber returns a block using the provided number
	GetBlockByNumber(num uint64, full bool) (*types.Block, bool)

	// ReadTxLookup returns a block hash in which a given txn was mined
	ReadTxLookup(txnHash types.Hash) (types.Hash, bool)

	// GetReceiptsByHash returns the receipts for a block hash
	GetReceiptsByHash(hash types.Hash) ([]*types.Receipt, error)
}

type syncer struct {
	logger hclog.Logger

	peerMap            *PeerMap
	syncAppPeerClient  SyncAppPeerClient
	syncAppPeerService SyncAppPeerService

	// Timeout for syncing a block
	blockTimeout time.Duration

	// Channel to notify Sync that a new status arrived
	newStatusCh chan struct{}

	blockchainStore  blockchainStore
	host             host.Host
	applicationStore ApplicationStore

	peersBlockNumMap map[peer.ID]uint64
}

type ValidatorStore interface {
	// Get current validators
	GetCurrentValidators() validators.Validators

	// Get singer address
	GetSignerAddress() types.Address
}

type Syncer interface {
	// Start starts syncer processes
	Start(topicSubFlag bool) error
	// Close terminates syncer process
	Close() error
	// GetAppPeer get AppPeer by PeerID
	GetAppPeer(id string) *AppPeer
}

func NewSyncer(
	logger hclog.Logger,
	syncAppPeerClient SyncAppPeerClient,
	syncAppPeerService SyncAppPeerService,
	host host.Host,
	blockchainStore blockchainStore,
	applicationStore ApplicationStore,
) Syncer {
	return &syncer{
		logger:             logger.Named(syncerName),
		syncAppPeerClient:  syncAppPeerClient,
		syncAppPeerService: syncAppPeerService,
		newStatusCh:        make(chan struct{}),
		peerMap:            new(PeerMap),
		host:               host,
		blockchainStore:    blockchainStore,
		applicationStore:   applicationStore,
		peersBlockNumMap:   make(map[peer.ID]uint64),
	}
}

// initializePeerMap fetches peer statuses and initializes map
func (s *syncer) initializePeerMap() {
	peerStatuses := s.syncAppPeerClient.GetConnectedPeerStatuses()
	s.peerMap.Put(peerStatuses...)
}

// Close terminates goroutine processes
func (s *syncer) Close() error {
	close(s.newStatusCh)

	if err := s.syncAppPeerService.Close(); err != nil {
		return err
	}

	s.syncAppPeerClient.Close()

	return nil
}

func (s *syncer) Start(topicSubFlag bool) error {
	if err := s.syncAppPeerClient.Start(topicSubFlag); err != nil {
		return err
	}

	s.syncAppPeerService.Start()

	go s.startPeerStatusUpdateProcess()
	//go s.startPeerConnectionEventProcess()
	go func() {
		s.doPublishAppStatus()
		ticker := time.NewTicker(DefaultAppStatusPublishDuration)
		for {
			<-ticker.C
			s.doPublishAppStatus()
		}
		ticker.Stop()
	}()

	return nil

}

func (s *syncer) doPublishAppStatus() {
	addr := ""
	if len(s.host.Addrs()) > 0 {
		addr = s.host.Addrs()[0].String()
	}
	s.syncAppPeerClient.PublishApplicationStatus(&appProto.AppStatus{
		Name:         s.applicationStore.GetEndpointApplication().Name,
		NodeId:       s.applicationStore.GetEndpointApplication().PeerID.String(),
		Uptime:       s.applicationStore.GetEndpointApplication().Uptime,
		StartupTime:  s.applicationStore.GetEndpointApplication().StartupTime,
		Relay:        "",
		Addr:         addr,
		AppOrigin:    s.applicationStore.GetEndpointApplication().AppOrigin,
		Mac:          s.applicationStore.GetEndpointApplication().Mac,
		CpuInfo:      s.applicationStore.GetEndpointApplication().CpuInfo,
		GpuInfo:      s.applicationStore.GetEndpointApplication().GpuInfo,
		MemInfo:      s.applicationStore.GetEndpointApplication().MemInfo,
		ModelHash:    s.applicationStore.GetEndpointApplication().ModelHash,
		AveragePower: s.applicationStore.GetEndpointApplication().AveragePower,
		Version:      s.applicationStore.GetEndpointApplication().Version,
	})

	s.logger.Debug("AppPeerStatus published ", "NodeID", s.applicationStore.GetEndpointApplication().PeerID.String(), "Addr", addr, "Mac", s.applicationStore.GetEndpointApplication().Mac)
}

// startPeerStatusUpdateProcess subscribes peer status change event and updates peer map
func (s *syncer) startPeerStatusUpdateProcess() {
	for peerStatus := range s.syncAppPeerClient.GetPeerStatusUpdateCh() {
		//s.logger.Debug("AppPeerStatus updated ", "NodeID", peerStatus.ID)

		// TODO validate peer status
		// store app in store
		s.putToPeerMap(peerStatus)
	}
}

// putToPeerMap puts given status to peer map
func (s *syncer) putToPeerMap(status *AppPeer) {
	s.peerMap.Put(status)
	s.notifyNewStatusEvent()
}

// putToPeerMap puts given status to peer map
func (s *syncer) GetAppPeer(id string) *AppPeer {
	return s.peerMap.Get(id)
}

// removeFromPeerMap removes the peer from peer map
func (s *syncer) removeFromPeerMap(peerID peer.ID) {
	s.peerMap.Remove(peerID)
}

// notifyNewStatusEvent emits signal to newStatusCh
func (s *syncer) notifyNewStatusEvent() {
	select {
	case s.newStatusCh <- struct{}{}:
	default:
	}
}
