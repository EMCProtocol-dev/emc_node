package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/emc-protocol/edge-matrix/application"
	"github.com/emc-protocol/edge-matrix/blockchain"
	"github.com/emc-protocol/edge-matrix/chain"
	cmdConfig "github.com/emc-protocol/edge-matrix/command/server/config"
	"github.com/emc-protocol/edge-matrix/consensus"
	"github.com/emc-protocol/edge-matrix/crypto"
	"github.com/emc-protocol/edge-matrix/helper/progress"
	"github.com/emc-protocol/edge-matrix/miner"
	minerProto "github.com/emc-protocol/edge-matrix/miner/proto"
	"github.com/emc-protocol/edge-matrix/relay"
	"github.com/emc-protocol/edge-matrix/rtc"
	rtcCrypto "github.com/emc-protocol/edge-matrix/rtc/crypto"
	"github.com/emc-protocol/edge-matrix/state"
	itrie "github.com/emc-protocol/edge-matrix/state/immutable-trie"
	"github.com/emc-protocol/edge-matrix/state/runtime"
	"github.com/emc-protocol/edge-matrix/telepool"
	"github.com/emc-protocol/edge-matrix/types"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/multiformats/go-multiaddr"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/emc-protocol/edge-matrix/helper/common"
	"github.com/emc-protocol/edge-matrix/jsonrpc"
	"github.com/emc-protocol/edge-matrix/network"
	"github.com/emc-protocol/edge-matrix/secrets"
	"github.com/emc-protocol/edge-matrix/server/proto"
	"github.com/hashicorp/go-hclog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

type RunningModeType string

const (
	RunningModeFull RunningModeType = "full"
	RunningModeEdge RunningModeType = "edge"
)
const (
	BaseDiscProto     = "/base/disc/0.1"
	BaseIdentityProto = "/base/id/0.1"
	EdgeDiscProto     = "/disc/0.2"
	EdgeIdentityProto = "/id/0.2"
)

// Server is the central manager of the blockchain client
type Server struct {
	logger       hclog.Logger
	config       *Config
	state        state.State
	stateStorage itrie.Storage

	consensus consensus.Consensus

	// blockchain stack
	blockchain *blockchain.Blockchain
	chain      *chain.Chain

	// state executor
	executor *state.Executor

	// jsonrpc stack
	jsonrpcServer *jsonrpc.JSONRPC

	// system grpc server
	grpcServer *grpc.Server

	// base libp2p network
	network *network.Server

	// edge libp2p network
	edgeNetwork *network.Server

	// relay client
	relayClient *relay.RelayClient

	// relay server
	relayServer *relay.RelayServer

	// application syncer Client
	syncAppPeerClient application.SyncAppPeerClient

	// telegram pool
	telepool *telepool.TelegramPool

	// secrets manager
	secretsManager secrets.SecretsManager

	// restore
	restoreProgression *progress.ProgressionWrapper

	// running mode
	runningMode RunningModeType
}

var dirPaths = []string{
	"blockchain",
	"trie",
}

// newFileLogger returns logger instance that writes all logs to a specified file.
// If log file can't be created, it returns an error
func newFileLogger(config *Config) (hclog.Logger, error) {
	logFileWriter, err := os.Create(config.LogFilePath)
	if err != nil {
		return nil, fmt.Errorf("could not create log file, %w", err)
	}

	return hclog.New(&hclog.LoggerOptions{
		Name:       "edge-matrix",
		Level:      config.LogLevel,
		Output:     logFileWriter,
		JSONFormat: config.JSONLogFormat,
	}), nil
}

// newCLILogger returns minimal logger instance that sends all logs to standard output
func newCLILogger(config *Config) hclog.Logger {
	return hclog.New(&hclog.LoggerOptions{
		Name:       "edge-matrix",
		Level:      config.LogLevel,
		JSONFormat: config.JSONLogFormat,
	})
}

// newLoggerFromConfig creates a new logger which logs to a specified file.
// If log file is not set it outputs to standard output ( console ).
// If log file is specified, and it can't be created the server command will error out
func newLoggerFromConfig(config *Config) (hclog.Logger, error) {
	if config.LogFilePath != "" {
		fileLoggerInstance, err := newFileLogger(config)
		if err != nil {
			return nil, err
		}

		return fileLoggerInstance, nil
	}

	return newCLILogger(config), nil
}

// NewServer creates a new Minimal server, using the passed in configuration
func NewServer(config *Config) (*Server, error) {
	logger, err := newLoggerFromConfig(config)
	if err != nil {
		return nil, fmt.Errorf("could not setup new logger instance, %w", err)
	}

	m := &Server{
		logger:             logger.Named("server"),
		config:             config,
		chain:              config.Chain,
		grpcServer:         grpc.NewServer(),
		restoreProgression: progress.NewProgressionWrapper(progress.ChainSyncRestore),
	}

	if m.config.RunningMode == cmdConfig.DefaultRunningMode {
		m.runningMode = RunningModeFull
	} else {
		m.runningMode = RunningModeEdge
	}
	m.logger.Info("Node running", "mode", m.runningMode)

	m.logger.Info("Data dir", "path", config.DataDir)

	// Generate all the paths in the dataDir
	if err := common.SetupDataDir(config.DataDir, dirPaths, 0770); err != nil {
		return nil, fmt.Errorf("failed to create data directories: %w", err)
	}

	// Set up datadog profiler
	if ddErr := m.enableDataDogProfiler(); err != nil {
		m.logger.Error("DataDog profiler setup failed", "err", ddErr.Error())
	}

	// Set up the secrets manager
	if err := m.setupSecretsManager(); err != nil {
		return nil, fmt.Errorf("failed to set up the secrets manager: %w", err)
	}

	// setup base libp2p network
	netConfig := config.Network
	netConfig.Chain = m.config.Chain
	netConfig.DataDir = filepath.Join(m.config.DataDir, "libp2p")
	netConfig.SecretsManager = m.secretsManager
	coreNetwork, err := network.NewServer(logger, netConfig, BaseDiscProto, BaseIdentityProto, false)
	if err != nil {
		return nil, err
	}
	m.network = coreNetwork

	// start blockchain object
	stateStorage, err := itrie.NewLevelDBStorage(filepath.Join(m.config.DataDir, "trie"), logger)
	if err != nil {
		return nil, err
	}

	m.stateStorage = stateStorage

	st := itrie.NewState(stateStorage)
	m.state = st

	m.executor = state.NewExecutor(config.Chain.Params, st, logger)

	//compute the genesis root state
	genesisRoot := m.executor.WriteGenesis(config.Chain.Genesis.Alloc)
	config.Chain.Genesis.StateRoot = genesisRoot

	//use the eip155 signer
	signer := crypto.NewEIP155Signer(chain.AllForksEnabled.At(0), uint64(m.config.Chain.Params.ChainID))

	//blockchain object
	m.blockchain, err = blockchain.NewBlockchain(logger, m.config.DataDir, config.Chain, nil, m.executor, signer)
	if err != nil {
		return nil, err
	}

	m.executor.GetHash = m.blockchain.GetHashHelper

	if m.runningMode == RunningModeFull {
		// setup edge libp2p network
		edgeNetConfig := config.EdgeNetwork
		edgeNetConfig.Chain = m.config.Chain
		edgeNetConfig.DataDir = filepath.Join(m.config.DataDir, "libp2p")
		edgeNetConfig.SecretsManager = m.secretsManager
		edgeNetwork, err := network.NewServer(logger.Named("edge"), edgeNetConfig, EdgeDiscProto, EdgeIdentityProto, true)
		if err != nil {
			return nil, err
		}
		m.edgeNetwork = edgeNetwork
	}

	{
		// Setup telegram pool
		hub := &telepoolHub{
			state:      m.state,
			Blockchain: m.blockchain,
		}

		// start transaction pool
		teleVersion := m.config.Chain.TeleVersion
		if teleVersion != "" {
			m.logger.Info("Tele proto", "version", m.config.Chain.TeleVersion)
		}
		m.telepool, err = telepool.NewTelegramPool(
			logger,
			hub,
			m.network,
			m.edgeNetwork,
			&telepool.Config{
				MaxSlots:           m.config.MaxSlots,
				MaxAccountEnqueued: m.config.MaxAccountEnqueued,
			},
			m.config.Chain.TeleVersion,
		)
		if err != nil {
			return nil, err
		}

		m.telepool.SetSigner(signer)

		// Setup consensus
		if err := m.setupConsensus(); err != nil {
			return nil, err
		}
		m.blockchain.SetConsensus(m.consensus)
	}

	{
		if m.runningMode == RunningModeFull {
			//after consensus is done, we can mine the genesis block in blockchain
			//This is done because consensus might use a custom Hash function so we need
			//to wait for consensus because we do any block hashing like genesis
			if err := m.blockchain.ComputeGenesis(); err != nil {
				return nil, err
			}

			//initialize data in consensus layer
			if err := m.consensus.Initialize(); err != nil {
				return nil, err
			}
		}
	}
	keyBytes, err := m.secretsManager.GetSecret(secrets.ValidatorKey)
	if err != nil {
		return nil, err
	}

	key, err := crypto.BytesToECDSAPrivateKey(keyBytes)
	if err != nil {
		return nil, err
	}
	//networkPrivKey, err := m.secretsManager.GetSecret(secrets.NetworkKey)
	//if err != nil {
	//	return nil, err
	//}
	//
	//decodedNetworkPrivKey, err := crypto.BytesToECDSAPrivateKey(networkPrivKey)

	minerAgent := miner.NewMinerHubAgent(m.logger, m.secretsManager)

	// init miner grpc service
	_, err = m.initMinerService(minerAgent, coreNetwork.GetHost(), m.secretsManager)
	if err != nil {
		return nil, err
	}

	// setup and start grpc server
	{
		if err := m.setupGRPC(); err != nil {
			return nil, err
		}
	}

	// start network
	{
		if m.runningMode == RunningModeFull {
			// start base network
			if err := m.network.Start("Base", m.config.Chain.BaseBootnodes); err != nil {
				return nil, err
			}

			// start consensus
			if err := m.consensus.Start(); err != nil {
				return nil, err
			}

			// start edge network
			if err := m.edgeNetwork.Start("Edge", m.config.Chain.Bootnodes); err != nil {
				return nil, err
			}
		}
	}

	{
		// setup edge application
		var endpointHost host.Host

		if m.edgeNetwork != nil {
			endpointHost = m.edgeNetwork.GetHost()
		}

		relayNetConfig := config.EdgeNetwork
		relayNetConfig.Chain = m.config.Chain
		relayNetConfig.DataDir = filepath.Join(m.config.DataDir, "libp2p")
		relayNetConfig.SecretsManager = m.secretsManager

		if m.runningMode == RunningModeEdge {
			// start edge network relay reserv
			relayClient, err := relay.NewRelayClient(logger, relayNetConfig, m.config.RelayOn)
			if err != nil {
				return nil, err
			}
			endpointHost = relayClient.GetHost()

			m.relayClient = relayClient
			if m.config.RelayOn {
				if err := relayClient.StartRelayReserv(); err != nil {
					return nil, err
				}
			}
		}

		endpoint, err := application.NewApplicationEndpoint(m.logger, key, endpointHost, m.config.AppName, m.config.AppUrl, m.blockchain, minerAgent, m.runningMode == RunningModeEdge)
		if err != nil {
			return nil, err
		}

		endpoint.SetSigner(application.NewEIP155Signer(chain.AllForksEnabled.At(0), uint64(m.config.Chain.Params.ChainID)))

		if m.runningMode == RunningModeEdge {
			// keep edge peer alive
			err := m.relayClient.StartAlive(endpoint.SubscribeEvents())
			if err != nil {
				return nil, err
			}
		}

		if m.runningMode == RunningModeFull {
			// setup app status syncer
			syncAppclient := application.NewSyncAppPeerClient(m.logger, m.edgeNetwork, minerAgent, m.edgeNetwork.GetHost(), endpoint)
			m.syncAppPeerClient = syncAppclient

			syncer := application.NewSyncer(
				m.logger,
				syncAppclient,
				application.NewSyncAppPeerService(m.logger, m.edgeNetwork, endpoint, m.blockchain, minerAgent),
				m.edgeNetwork.GetHost(),
				m.blockchain,
				endpoint)
			// start app status syncer
			err = syncer.Start(true)
			if err != nil {
				return nil, err
			}

			// setup and start jsonrpc server
			if err := m.setupJSONRPC(); err != nil {
				return nil, err
			}

			// start relay server
			if config.RelayAddr.Port > 0 {
				relayListenAddr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", config.RelayAddr.IP.String(), config.RelayAddr.Port))
				if err != nil {
					return nil, err
				}
				relayServer, err := relay.NewRelayServer(logger, m.secretsManager, relayListenAddr, relayNetConfig, config.RelayDiscovery)
				if err != nil {
					return nil, err
				}
				logger.Info("LibP2P Relay server running", "addr", relayListenAddr.String()+"/p2p/"+relayServer.GetHost().ID().String())

				// setup relay libp2p network
				//relayNetConfig := config.EdgeNetwork
				//relayNetConfig.Chain = m.config.Chain
				//relayNetConfig.DataDir = filepath.Join(m.config.DataDir, "libp2p")
				//relayNetConfig.SecretsManager = m.secretsManager
				//relayNetConfig.Addr = &net.TCPAddr{
				//	IP:   net.ParseIP(config.RelayAddr.IP.String()),
				//	Port: config.RelayAddr.Port,
				//}
				//relayNetwork, err := network.NewServer(logger.Named("Relay"), relayNetConfig, EdgeDiscProto, EdgeIdentityProto, true)
				//if err != nil {
				//	return nil, err
				//}
				//relayNetwork.StartMininum("Relay")
				//relayServer, err := relay.NewRelayServerWithHost(logger, relayNetwork.GetHost())
				//if err != nil {
				//	return nil, err
				//}

				err = relayServer.SetupAliveService(syncAppclient)
				if err != nil {
					return nil, fmt.Errorf("unable to setup alive service, %w", err)
				}

				m.relayServer = relayServer

			}

			// start edge-network alive gossip
			//err := m.edgeNetwork.StartPeerAliveGossip()
			//if err != nil {
			//	return nil, err
			//}

			// start telepool
			m.telepool.SetAppSyncer(syncer)
			m.telepool.Start()

		}
	}

	return m, nil
}

//func (s *Server) restoreChain() error {
//	if s.config.RestoreFile == nil {
//		return nil
//	}
//
//	if err := archive.RestoreChain(s.blockchain, *s.config.RestoreFile, s.restoreProgression); err != nil {
//		return err
//	}
//
//	return nil
//}

type telepoolHub struct {
	state state.State
	*blockchain.Blockchain
}

// getAccountImpl is used for fetching account state from both TxPool and JSON-RPC
func getAccountImpl(state state.State, root types.Hash, addr types.Address) (*state.Account, error) {
	snap, err := state.NewSnapshotAt(root)
	if err != nil {
		return nil, fmt.Errorf("unable to get snapshot for root '%s': %w", root, err)
	}

	account, err := snap.GetAccount(addr)
	if err != nil {
		return nil, err
	}

	if account == nil {
		return nil, jsonrpc.ErrStateNotFound
	}

	return account, nil
}

func (t *telepoolHub) GetNonce(root types.Hash, addr types.Address) uint64 {
	account, err := getAccountImpl(t.state, root, addr)

	if err != nil {
		return 0
	}

	return account.Nonce
}

func (t *telepoolHub) GetBalance(root types.Hash, addr types.Address) (*big.Int, error) {
	account, err := getAccountImpl(t.state, root, addr)

	if err != nil {
		if errors.Is(err, jsonrpc.ErrStateNotFound) {
			return big.NewInt(0), nil
		}

		return big.NewInt(0), err
	}

	return account.Balance, nil
}

// setupSecretsManager sets up the secrets manager
func (s *Server) setupSecretsManager() error {
	secretsManagerConfig := s.config.SecretsManager
	if secretsManagerConfig == nil {
		// No config provided, use default
		secretsManagerConfig = &secrets.SecretsManagerConfig{
			Type: secrets.Local,
		}
	}

	secretsManagerType := secretsManagerConfig.Type
	secretsManagerParams := &secrets.SecretsManagerParams{
		Logger: s.logger,
	}

	if secretsManagerType == secrets.Local {
		// Only the base directory is required for
		// the local secrets manager
		secretsManagerParams.Extra = map[string]interface{}{
			secrets.Path: s.config.DataDir,
		}
	}

	// Grab the factory method
	secretsManagerFactory, ok := secretsManagerBackends[secretsManagerType]
	if !ok {
		return fmt.Errorf("secrets manager type '%s' not found", secretsManagerType)
	}

	// Instantiate the secrets manager
	secretsManager, factoryErr := secretsManagerFactory(
		secretsManagerConfig,
		secretsManagerParams,
	)

	if factoryErr != nil {
		return fmt.Errorf("unable to instantiate secrets manager, %w", factoryErr)
	}

	s.secretsManager = secretsManager

	return nil
}

// setupConsensus sets up the consensus mechanism
func (s *Server) setupConsensus() error {
	engineName := s.config.Chain.Params.GetEngine()
	engine, ok := consensusBackends[ConsensusType(engineName)]

	if !ok {
		return fmt.Errorf("consensus engine '%s' not found", engineName)
	}

	engineConfig, ok := s.config.Chain.Params.Engine[engineName].(map[string]interface{})
	if !ok {
		engineConfig = map[string]interface{}{}
	}

	config := &consensus.Config{
		Params: s.config.Chain.Params,
		Config: engineConfig,
		Path:   filepath.Join(s.config.DataDir, "consensus"),
	}

	chainConsensus, err := engine(
		&consensus.Params{
			Context:               context.Background(),
			Config:                config,
			TelePool:              s.telepool,
			Network:               s.network,
			Blockchain:            s.blockchain,
			Executor:              s.executor,
			Grpc:                  s.grpcServer,
			Logger:                s.logger,
			SecretsManager:        s.secretsManager,
			BlockTime:             s.config.BlockTime,
			NumBlockConfirmations: s.config.NumBlockConfirmations,
		},
	)

	if err != nil {
		return err
	}

	s.consensus = chainConsensus

	return nil
}

// initMinerService sets up the Miner grpc service
func (s *Server) initMinerService(minerAgent *miner.MinerHubAgent, host host.Host, secretsManager secrets.SecretsManager) (*miner.MinerService, error) {
	if s.grpcServer != nil {
		minerService := miner.NewMinerService(s.logger, minerAgent, host, secretsManager)
		minerProto.RegisterMinerServer(s.grpcServer, minerService)
		return minerService, nil
	}

	return nil, errors.New("grpcServer is nil")
}

// setupRelayer sets up the relayer
//func (s *Server) setupRelayer() error {
//	account, err := wallet.NewAccountFromSecret(s.secretsManager)
//	if err != nil {
//		return fmt.Errorf("failed to create account from secret: %w", err)
//	}
//
//	relayer := statesyncrelayer.NewRelayer(
//		s.config.DataDir,
//		s.config.JSONRPC.JSONRPCAddr.String(),
//		ethgo.Principal(contracts.StateReceiverContract),
//		s.logger.Named("relayer"),
//		wallet.NewEcdsaSigner(wallet.NewKey(account, bls.DomainCheckpointManager)),
//	)
//
//	// start relayer
//	if err := relayer.Start(); err != nil {
//		return fmt.Errorf("failed to start relayer: %w", err)
//	}
//
//	return nil
//}

type jsonRPCHub struct {
	state              state.State
	restoreProgression *progress.ProgressionWrapper

	*blockchain.Blockchain
	*telepool.TelegramPool
	*state.Executor
	*network.Server
	consensus.Consensus
	*rtc.Rtc
	application.SyncAppPeerClient
	//consensus.BridgeDataProvider
}

func (j *jsonRPCHub) SendMsg(msg *rtc.RtcMsg) error {
	return j.AddRtcMsg(msg)
}

func (j *jsonRPCHub) GetPeers() int {
	return len(j.Server.Peers())
}

func (j *jsonRPCHub) GetAccount(root types.Hash, addr types.Address) (*jsonrpc.Account, error) {
	acct, err := getAccountImpl(j.state, root, addr)
	if err != nil {
		return nil, err
	}

	account := &jsonrpc.Account{
		Nonce:   acct.Nonce,
		Balance: new(big.Int).Set(acct.Balance),
	}

	return account, nil
}

// GetForksInTime returns the active forks at the given block height
func (j *jsonRPCHub) GetForksInTime(blockNumber uint64) chain.ForksInTime {
	return j.Executor.GetForksInTime(blockNumber)
}

func (j *jsonRPCHub) GetStorage(stateRoot types.Hash, addr types.Address, slot types.Hash) ([]byte, error) {
	account, err := getAccountImpl(j.state, stateRoot, addr)
	if err != nil {
		return nil, err
	}

	snap, err := j.state.NewSnapshotAt(stateRoot)
	if err != nil {
		return nil, err
	}

	res := snap.GetStorage(addr, account.Root, slot)

	return res.Bytes(), nil
}

func (j *jsonRPCHub) GetCode(root types.Hash, addr types.Address) ([]byte, error) {
	account, err := getAccountImpl(j.state, root, addr)
	if err != nil {
		return nil, err
	}

	code, ok := j.state.GetCode(types.BytesToHash(account.CodeHash))
	if !ok {
		return nil, fmt.Errorf("unable to fetch code")
	}

	return code, nil
}

func (j *jsonRPCHub) ApplyTxn(
	header *types.Header,
	txn *types.Telegram,
) (result *runtime.ExecutionResult, err error) {
	blockCreator, err := j.GetConsensus().GetBlockCreator(header)
	if err != nil {
		return nil, err
	}

	transition, err := j.BeginTxn(header.StateRoot, header, blockCreator)
	if err != nil {
		return
	}

	result, err = transition.Apply(txn)

	return
}

// TraceBlock traces all transactions in the given block and returns all results
//func (j *jsonRPCHub) TraceBlock(
//	block *types.Block,
//	tracer tracer.Tracer,
//) ([]interface{}, error) {
//	if block.Number() == 0 {
//		return nil, errors.New("genesis block can't have transaction")
//	}
//
//	parentHeader, ok := j.GetHeaderByHash(block.ParentHash())
//	if !ok {
//		return nil, errors.New("parent header not found")
//	}
//
//	blockCreator, err := j.GetConsensus().GetBlockCreator(block.Header)
//	if err != nil {
//		return nil, err
//	}
//
//	transition, err := j.BeginTxn(parentHeader.StateRoot, block.Header, blockCreator)
//	if err != nil {
//		return nil, err
//	}
//
//	transition.SetTracer(tracer)
//
//	results := make([]interface{}, len(block.Transactions))
//
//	for idx, tx := range block.Transactions {
//		tracer.Clear()
//
//		if _, err := transition.Apply(tx); err != nil {
//			return nil, err
//		}
//
//		if results[idx], err = tracer.GetResult(); err != nil {
//			return nil, err
//		}
//	}
//
//	return results, nil
//}

// TraceTxn traces a transaction in the block, associated with the given hash
//func (j *jsonRPCHub) TraceTxn(
//	block *types.Block,
//	targetTxHash types.Hash,
//	tracer tracer.Tracer,
//) (interface{}, error) {
//	if block.Number() == 0 {
//		return nil, errors.New("genesis block can't have transaction")
//	}
//
//	parentHeader, ok := j.GetHeaderByHash(block.ParentHash())
//	if !ok {
//		return nil, errors.New("parent header not found")
//	}
//
//	blockCreator, err := j.GetConsensus().GetBlockCreator(block.Header)
//	if err != nil {
//		return nil, err
//	}
//
//	transition, err := j.BeginTxn(parentHeader.StateRoot, block.Header, blockCreator)
//	if err != nil {
//		return nil, err
//	}
//
//	var targetTx *types.Transaction
//
//	for _, tx := range block.Transactions {
//		if tx.Hash == targetTxHash {
//			targetTx = tx
//
//			break
//		}
//
//		// Execute transactions without tracer until reaching the target transaction
//		if _, err := transition.Apply(tx); err != nil {
//			return nil, err
//		}
//	}
//
//	if targetTx == nil {
//		return nil, errors.New("target tx not found")
//	}
//
//	transition.SetTracer(tracer)
//
//	if _, err := transition.Apply(targetTx); err != nil {
//		return nil, err
//	}
//
//	return tracer.GetResult()
//}

//func (j *jsonRPCHub) TraceCall(
//	tx *types.Transaction,
//	parentHeader *types.Header,
//	tracer tracer.Tracer,
//) (interface{}, error) {
//	blockCreator, err := j.GetConsensus().GetBlockCreator(parentHeader)
//	if err != nil {
//		return nil, err
//	}
//
//	transition, err := j.BeginTxn(parentHeader.StateRoot, parentHeader, blockCreator)
//	if err != nil {
//		return nil, err
//	}
//
//	transition.SetTracer(tracer)
//
//	if _, err := transition.Apply(tx); err != nil {
//		return nil, err
//	}
//
//	return tracer.GetResult()
//}

func (j *jsonRPCHub) GetSyncProgression() *progress.Progression {
	// restore progression
	if restoreProg := j.restoreProgression.GetProgression(); restoreProg != nil {
		return restoreProg
	}

	// consensus sync progression
	if consensusSyncProg := j.Consensus.GetSyncProgression(); consensusSyncProg != nil {
		return consensusSyncProg
	}

	return nil
}

// SETUP //

// setupJSONRCP sets up the JSONRPC server, using the set configuration
func (s *Server) setupJSONRPC() error {
	hub := &jsonRPCHub{
		state:              s.state,
		restoreProgression: s.restoreProgression,
		Blockchain:         s.blockchain,
		TelegramPool:       s.telepool,
		Executor:           s.executor,
		Consensus:          s.consensus,
		Server:             s.network,
		SyncAppPeerClient:  s.syncAppPeerClient,
		//BridgeDataProvider: s.consensus.GetBridgeProvider(),
	}
	rt, err := rtc.NewRtc(s.network, s.logger)
	if err != nil {
		return err
	}
	//rt.SetSigner(rtcCrypto.NewRtcSigner(uint64(s.config.Chain.Params.ChainID)))
	rt.SetSigner(rtcCrypto.NewEIP155Signer(chain.AllForksEnabled.At(0), uint64(s.config.Chain.Params.ChainID)))
	hub.Rtc = rt
	conf := &jsonrpc.Config{
		Store:                    hub,
		Addr:                     s.config.JSONRPC.JSONRPCAddr,
		ChainID:                  uint64(s.config.Chain.Params.ChainID),
		ChainName:                s.chain.Name,
		AccessControlAllowOrigin: s.config.JSONRPC.AccessControlAllowOrigin,
		PriceLimit:               s.config.PriceLimit,
		BatchLengthLimit:         s.config.JSONRPC.BatchLengthLimit,
		BlockRangeLimit:          s.config.JSONRPC.BlockRangeLimit,
	}

	srv, err := jsonrpc.NewJSONRPC(s.logger, conf)
	if err != nil {
		return err
	}

	s.jsonrpcServer = srv

	return nil
}

// setupGRPC sets up the grpc server and listens on tcp
func (s *Server) setupGRPC() error {
	proto.RegisterSystemServer(s.grpcServer, &systemService{server: s})

	lis, err := net.Listen("tcp", s.config.GRPCAddr.String())
	if err != nil {
		return err
	}

	go func() {
		if err := s.grpcServer.Serve(lis); err != nil {
			s.logger.Error(err.Error())
		}
	}()

	s.logger.Info("GRPC server running", "addr", s.config.GRPCAddr.String())

	return nil
}

// Chain returns the chain object of the client
//func (s *Server) Chain() *chain.Chain {
//	return s.chain
//}

// JoinPeer attempts to add a new peer to the networking server
func (s *Server) JoinPeer(rawPeerMultiaddr string) error {
	return s.network.JoinPeer(rawPeerMultiaddr)
}

// Close closes the Minimal server (blockchain, networking, consensus)
func (s *Server) Close() {
	// Close the blockchain layer
	//if err := s.blockchain.Close(); err != nil {
	//	s.logger.Error("failed to close blockchain", "err", err.Error())
	//}

	// Close the networking layer
	if err := s.network.Close(); err != nil {
		s.logger.Error("failed to close networking", "err", err.Error())
	}

	// Close the consensus layer
	//if err := s.consensus.Close(); err != nil {
	//	s.logger.Error("failed to close consensus", "err", err.Error())
	//}

	// Close the state storage
	//if err := s.stateStorage.Close(); err != nil {
	//	s.logger.Error("failed to close storage for trie", "err", err.Error())
	//}

	//if s.prometheusServer != nil {
	//	if err := s.prometheusServer.Shutdown(context.Background()); err != nil {
	//		s.logger.Error("Prometheus server shutdown error", err)
	//	}
	//}

	// Stop state sync relayer
	//if s.stateSyncRelayer != nil {
	//	s.stateSyncRelayer.Stop()
	//}

	// Close the txpool's main loop
	//s.txpool.Close()

	// Close DataDog profiler
	s.closeDataDogProfiler()
}

// Entry is a consensus configuration entry
type Entry struct {
	Enabled bool
	Config  map[string]interface{}
}

func (s *Server) startPrometheusServer(listenAddr *net.TCPAddr) *http.Server {
	srv := &http.Server{
		Addr: listenAddr.String(),
		Handler: promhttp.InstrumentMetricHandler(
			prometheus.DefaultRegisterer, promhttp.HandlerFor(
				prometheus.DefaultGatherer,
				promhttp.HandlerOpts{},
			),
		),
		ReadHeaderTimeout: 60 * time.Second,
	}

	go func() {
		s.logger.Info("Prometheus server started", "addr=", listenAddr.String())

		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("Prometheus HTTP server ListenAndServe", "err", err)
		}
	}()

	return srv
}
