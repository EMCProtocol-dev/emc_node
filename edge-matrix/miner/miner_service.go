package miner

import (
	"context"
	"github.com/emc-protocol/edge-matrix/crypto"
	"github.com/emc-protocol/edge-matrix/helper/ic/utils/identity"
	"github.com/emc-protocol/edge-matrix/helper/ic/utils/principal"
	"github.com/emc-protocol/edge-matrix/miner/proto"
	"github.com/emc-protocol/edge-matrix/secrets"
	"github.com/hashicorp/go-hclog"
	"github.com/libp2p/go-libp2p/core/host"
	"google.golang.org/protobuf/types/known/emptypb"
)

const (
	setOpt    = "set"
	removeOpt = "remove"
)

type MinerService struct {
	proto.UnimplementedMinerServer
	logger         hclog.Logger
	icHost         string
	host           host.Host
	secretsManager secrets.SecretsManager

	// agent for communicating with IC Miner Canister
	minerAgent *MinerAgent
}

func NewMinerService(logger hclog.Logger, minerAgent *MinerAgent, host host.Host, secretsManager secrets.SecretsManager) *MinerService {
	return &MinerService{
		logger:         logger,
		minerAgent:     minerAgent,
		host:           host,
		secretsManager: secretsManager,
	}
}

// GetMiner return miner's status from secretsManager and IC canister
func (s *MinerService) GetMiner() (*proto.MinerStatus, error) {
	// query node from IC canister
	nodeId, nodeIdentity, wallet, registered, ntype, err := s.minerAgent.MyNode(s.host.ID().String())
	if err != nil {
		return nil, err
	}
	nodeType := ""
	if ntype > -1 {
		switch NodeType(ntype) {
		case NodeTypeRouter:
			nodeType = "router"
		case NodeTypeValidator:
			nodeType = "validator"
		case NodeTypeComputing:
			nodeType = "computing"
		default:
		}
	}

	status := proto.MinerStatus{
		NetName:      "IC",
		NodeId:       nodeId,
		NodeIdentity: nodeIdentity,
		Principal:    wallet,
		NodeType:     nodeType,
		Registered:   registered,
	}
	return &status, nil
}

func (s *MinerService) GetCurrentEPower(context.Context, *emptypb.Empty) (*proto.CurrentEPower, error) {
	round, power, err := s.minerAgent.MyCurrentEPower(s.host.ID().String())
	if err != nil {
		return nil, err
	}
	_, _, multiple, err := s.minerAgent.MyStack(s.host.ID().String())
	if err != nil {
		return nil, err
	}

	ePower := proto.CurrentEPower{
		Round:    round,
		Total:    power,
		Multiple: float32(multiple) / 10000.0,
	}
	return &ePower, nil
}

// PeersStatus implements the 'peers status' operator service
func (s *MinerService) GetMinerStatus(context.Context, *emptypb.Empty) (*proto.MinerStatus, error) {
	return s.GetMiner()
}

func (s *MinerService) GetIdentity() *identity.Identity {
	icPrivKey, err := s.secretsManager.GetSecret(secrets.ICPIdentityKey)
	if err != nil {
		return nil
	}
	decodedPrivKey, err := crypto.BytesToEd25519PrivateKey(icPrivKey)
	identity := identity.New(false, decodedPrivKey.Seed())
	return identity
}

// Regiser set or remove a principal for miner
func (s *MinerService) MinerRegiser(ctx context.Context, req *proto.MinerRegisterRequest) (*proto.MinerRegisterResponse, error) {
	identity := s.GetIdentity()
	p := principal.NewSelfAuthenticating(identity.PubKeyBytes())
	s.logger.Info("MinerRegiser", "node identity", p.Encode(), "NodeId", s.host.ID().String(), "Principal", req.Principal)

	result := ""
	if req.Commit == setOpt {
		result = "register ok"
		err := s.minerAgent.RegisterNode(NodeType(req.Type), s.host.ID().String(), req.Principal)
		if err != nil {
			result = err.Error()
		}
	} else if req.Commit == removeOpt {
		result = "unregister ok"
		err := s.minerAgent.UnRegisterNode(s.host.ID().String())
		if err != nil {
			result = err.Error()
		}
	}
	// TODO update minerFlag in application endpoint

	response := proto.MinerRegisterResponse{
		Message: result,
	}
	return &response, nil
}
