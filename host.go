package redwood

import (
	"context"
	"encoding/json"
	"math/rand"

	"github.com/pkg/errors"

	"github.com/brynbellomy/redwood/ctx"
)

type Host interface {
	ctx.Logger
	Ctx() *ctx.Context
	Start() error

	Subscribe(ctx context.Context, url string) error
	AddTx(ctx context.Context, tx Tx) error
	AddPeer(ctx context.Context, multiaddrString string) error
	Port() uint
	Transport() Transport
	Store() Store
	Address() Address
}

type host struct {
	*ctx.Context

	port              uint
	transport         Transport
	store             Store
	signingKeypair    *SigningKeypair
	encryptingKeypair *EncryptingKeypair

	subscriptionsOut map[string]subscriptionOut
	peerSeenTxs      map[string]map[Hash]bool
}

var (
	ErrUnsignedTx = errors.New("unsigned tx")
	ErrProtocol   = errors.New("protocol error")
)

func NewHost(signingKeypair *SigningKeypair, encryptingKeypair *EncryptingKeypair, port uint, store Store) (Host, error) {
	h := &host{
		Context:           &ctx.Context{},
		port:              port,
		store:             store,
		signingKeypair:    signingKeypair,
		encryptingKeypair: encryptingKeypair,
		subscriptionsOut:  make(map[string]subscriptionOut),
		peerSeenTxs:       make(map[string]map[Hash]bool),
	}
	return h, nil
}

func (h *host) Ctx() *ctx.Context {
	return h.Context
}

func (h *host) Start() error {
	return h.CtxStart(
		// on startup
		func() error {
			h.SetLogLabel(h.Address().Pretty() + " host")

			// transport, err := NewLibp2pTransport(h.Address(), h.port)
			transport, err := NewHTTPTransport(h.Address(), h.port, h.store)
			if err != nil {
				return err
			}

			transport.SetPutHandler(h.onTxReceived)
			transport.SetAckHandler(h.onAckReceived)
			transport.SetVerifyAddressHandler(h.onVerifyAddressReceived)
			h.transport = transport

			h.CtxAddChild(h.transport.Ctx(), nil)
			h.CtxAddChild(h.store.Ctx(), nil)

			err = h.store.Start()
			if err != nil {
				return err
			}
			return h.transport.Start()
		},
		nil,
		nil,
		// on shutdown
		func() {},
	)
}

func (h *host) Port() uint {
	return h.port
}

func (h *host) Transport() Transport {
	return h.transport
}

func (h *host) Store() Store {
	return h.store
}

func (h *host) Address() Address {
	return h.signingKeypair.Address()
}

func (h *host) onTxReceived(tx Tx, peer Peer) {
	h.Infof(0, "tx %v received", tx.Hash().Pretty())
	h.markTxSeenByPeer(peer.ID(), tx.Hash())

	// @@TODO: private txs

	if !h.store.HaveTx(tx.Hash()) {
		// Add to store
		err := h.store.AddTx(&tx)
		if err != nil {
			h.Errorf("error adding tx to store: %v", err)
		}

		// Broadcast to subscribed peers
		err = h.put(context.TODO(), tx)
		if err != nil {
			h.Errorf("error rebroadcasting tx: %v", err)
		}
	}

	err := peer.WriteMsg(Msg{Type: MsgType_Ack, Payload: tx.Hash()})
	if err != nil {
		h.Errorf("error ACKing peer: %v", err)
	}
}

func (h *host) onAckReceived(txHash Hash, peer Peer) {
	h.Infof(0, "ack received for %v from %v", txHash, peer.ID())
	h.markTxSeenByPeer(peer.ID(), txHash)
}

func (h *host) markTxSeenByPeer(peerID string, txHash Hash) {
	if h.peerSeenTxs[peerID] == nil {
		h.peerSeenTxs[peerID] = make(map[Hash]bool)
	}
	h.peerSeenTxs[peerID][txHash] = true
}

func (h *host) AddPeer(ctx context.Context, multiaddrString string) error {
	return h.transport.AddPeer(ctx, multiaddrString)
}

func (h *host) Subscribe(ctx context.Context, url string) error {
	_, exists := h.subscriptionsOut[url]
	if exists {
		return errors.New("already subscribed to " + url)
	}

	var peer Peer

	// @@TODO: subscribe to more than one peer?
	err := h.transport.ForEachProviderOfURL(ctx, url, func(p Peer) (bool, error) {
		err := p.EnsureConnected(ctx)
		if err != nil {
			return true, err
		}
		peer = p
		return false, nil
	})
	if err != nil {
		return errors.WithStack(err)
	} else if peer == nil {
		return errors.WithStack(ErrNoPeersForURL)
	}

	err = peer.WriteMsg(Msg{Type: MsgType_Subscribe, Payload: url})
	if err != nil {
		return errors.WithStack(err)
	}

	chDone := make(chan struct{})
	h.subscriptionsOut[url] = subscriptionOut{peer, chDone}

	go func() {
		defer peer.CloseConn()
		for {
			select {
			case <-chDone:
				return
			default:
			}

			msg, err := peer.ReadMsg()
			if err != nil {
				h.Errorf("error reading: %v", err)
				return
			}

			if msg.Type != MsgType_Put {
				panic("protocol error")
			}

			tx := msg.Payload.(Tx)
			tx.URL = url
			h.onTxReceived(tx, peer)

			// @@TODO: ACK the PUT
		}
	}()

	return nil
}

func (h *host) verifyPeerAddress(peer Peer, address Address) (EncryptingPublicKey, error) {
	challengeMsg := make([]byte, 128)
	_, err := rand.Read(challengeMsg)
	if err != nil {
		return nil, err
	}

	err = peer.WriteMsg(Msg{Type: MsgType_VerifyAddress, Payload: challengeMsg})
	if err != nil {
		return nil, err
	}

	msg, err := peer.ReadMsg()
	if err != nil {
		return nil, err
	} else if msg.Type != MsgType_VerifyAddressResponse {
		return nil, ErrProtocol
	}

	resp, ok := msg.Payload.(VerifyAddressResponse)
	if !ok {
		return nil, ErrProtocol
	}

	hash := HashBytes(resp.Signature)

	pubkey, err := RecoverSigningPubkey(hash, resp.Signature)
	if err != nil {
		return nil, err
	} else if pubkey.Address() != address {
		return nil, ErrInvalidSignature
	}

	return EncryptingPublicKeyFromBytes(resp.EncryptingPublicKey), nil
}

func (h *host) onVerifyAddressReceived(challengeMsg []byte) (VerifyAddressResponse, error) {
	hash := HashBytes(challengeMsg)
	sig, err := h.signingKeypair.SignHash(hash)
	if err != nil {
		return VerifyAddressResponse{}, err
	}
	return VerifyAddressResponse{
		Signature:           sig,
		EncryptingPublicKey: h.encryptingKeypair.EncryptingPublicKey.Bytes(),
	}, nil
}

func (h *host) peerWithAddress(ctx context.Context, address Address) (Peer, EncryptingPublicKey, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	chPeers, err := h.transport.PeersWithAddress(ctx, address)
	if err != nil {
		return nil, nil, err
	}

	for peer := range chPeers {
		err = peer.EnsureConnected(context.TODO())
		if err != nil {
			return nil, nil, err
		}
		defer peer.CloseConn()

		encryptingPubkey, err := h.verifyPeerAddress(peer, address)
		if err != nil {
			continue
		}

		return peer, encryptingPubkey, nil
	}
	return nil, nil, nil
}

func (h *host) put(ctx context.Context, tx Tx) error {
	// @@TODO: should we also send all PUTs to some set of authoritative peers (like a central server)?

	if len(tx.Sig) == 0 {
		return ErrUnsignedTx
	}

	if len(tx.Recipients) > 0 {
		marshalledTx, err := json.Marshal(tx)
		if err != nil {
			return err
		}

		for _, recipientAddr := range tx.Recipients {
			err := func() error {
				peer, encryptingPubkey, err := h.peerWithAddress(ctx, recipientAddr)
				if err != nil {
					return err
				} else if peer == nil {
					h.Errorf("couldn't find peer with address %s", recipientAddr)
					return nil
				}

				err = peer.EnsureConnected(context.TODO())
				if err != nil {
					return err
				}
				defer peer.CloseConn()

				msgEncrypted, err := h.encryptingKeypair.SealMessageFor(encryptingPubkey, marshalledTx)
				if err != nil {
					return err
				}

				err = peer.WriteMsg(Msg{Type: MsgType_Private, Payload: msgEncrypted})
				if err != nil {
					return err
				}
				return nil
			}()
			if err != nil {
				return err
			}

			// @@TODO: wait for ack?
		}

	} else {
		// @@TODO: do we need to trim the tx's patches' keypaths so that they don't include
		// the keypath that the subscription is listening to?

		err := h.transport.ForEachSubscriberToURL(ctx, tx.URL, func(peer Peer) (bool, error) {
			if h.peerSeenTxs[peer.ID()][tx.Hash()] {
				return true, nil
			}

			err := peer.EnsureConnected(context.TODO())
			if err != nil {
				// @@TODO: just log, don't break?
				return true, errors.WithStack(err)
			}

			err = peer.WriteMsg(Msg{Type: MsgType_Put, Payload: tx})
			if err != nil {
				// @@TODO: just log, don't break?
				return true, errors.WithStack(err)
			}
			return true, nil
		})
		return err
	}
	return nil
}

// @@TODO: remove this and build a mechanism for transports to fetch the public key associated with a given address
var ENCRYPTION_PUBKEY_FOR_ADDRESS map[Address]EncryptingPublicKey

func (h *host) AddTx(ctx context.Context, tx Tx) error {
	h.Info(0, "adding tx ", tx.Hash().Pretty())

	if len(tx.Sig) == 0 {
		err := h.SignTx(&tx)
		if err != nil {
			return err
		}
	}

	err := h.store.AddTx(&tx)
	if err != nil {
		return err
	}

	err = h.put(h.Ctx(), tx)
	if err != nil {
		return err
	}

	return nil
}

func (h *host) SignTx(tx *Tx) error {
	var err error
	tx.Sig, err = h.signingKeypair.SignHash(tx.Hash())
	return err
}
