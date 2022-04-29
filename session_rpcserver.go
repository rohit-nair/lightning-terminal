package terminal

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightninglabs/lightning-node-connect/mailbox"
	"github.com/lightninglabs/lightning-terminal/litrpc"
	"github.com/lightninglabs/lightning-terminal/session"
)

// sessionRpcServer is the gRPC server for the Session RPC interface.
type sessionRpcServer struct {
	litrpc.UnimplementedSessionsServer

	basicAuth string

	db            *session.DB
	sessionServer *session.Server

	superMacBaker func(ctx context.Context, rootKeyID uint64,
		recipe *session.MacaroonRecipe) (string, error)

	quit     chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once
}

// stop cleans up any sessionRpcServer resources.
func (s *sessionRpcServer) stop() {
	s.stopOnce.Do(func() {
		close(s.quit)
		s.wg.Wait()
	})
}

// AddSession adds and starts a new Terminal Connect session.
func (s *sessionRpcServer) AddSession(_ context.Context,
	req *litrpc.AddSessionRequest) (*litrpc.AddSessionResponse, error) {

	expiry := time.Unix(int64(req.ExpiryTimestampSeconds), 0)
	if time.Now().After(expiry) {
		return nil, fmt.Errorf("expiry must be in the future")
	}

	typ, err := unmarshalRPCType(req.SessionType)
	if err != nil {
		return nil, err
	}

	if typ != session.TypeUIPassword && typ != session.TypeMacaroonAdmin &&
		typ != session.TypeMacaroonReadonly {

		return nil, fmt.Errorf("invalid session type, only UI " +
			"password, admin and readonly macaroon types " +
			"supported in LiT")
	}

	sess, err := session.NewSession(
		req.Label, typ, expiry, req.MailboxServerAddr, req.DevServer,
		nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("error creating new session: %v", err)
	}

	if err := s.db.StoreSession(sess); err != nil {
		return nil, fmt.Errorf("error storing session: %v", err)
	}

	if err := s.resumeSession(sess); err != nil {
		return nil, fmt.Errorf("error starting session: %v", err)
	}

	rpcSession, err := marshalRPCSession(sess)
	if err != nil {
		return nil, fmt.Errorf("error marshaling session: %v", err)
	}

	return &litrpc.AddSessionResponse{
		Session: rpcSession,
	}, nil
}

// resumeSession tries to start an existing session if it is not expired, not
// revoked and a LiT session.
func (s *sessionRpcServer) resumeSession(sess *session.Session) error {
	pubKey := sess.LocalPublicKey
	pubKeyBytes := pubKey.SerializeCompressed()

	// We only start non-revoked, non-expired LiT sessions. Everything else
	// we just skip.
	if sess.State != session.StateInUse &&
		sess.State != session.StateCreated {

		log.Debugf("Not resuming session %x with state %d", pubKeyBytes,
			sess.State)
		return nil
	}

	// Don't resume an expired session.
	if sess.Expiry.Before(time.Now()) {
		log.Debugf("Not resuming session %x with expiry %s",
			pubKeyBytes, sess.Expiry)

		if err := s.db.RevokeSession(pubKey); err != nil {
			return fmt.Errorf("error revoking session: %v", err)
		}

		return nil
	}

	var authData []byte
	switch sess.Type {
	case session.TypeUIPassword:
		authData = []byte("Authorization: Basic " + s.basicAuth)

	case session.TypeMacaroonAdmin, session.TypeMacaroonReadonly:
		ctx := context.Background()
		readOnly := sess.Type == session.TypeMacaroonReadonly
		mac, err := s.superMacBaker(
			ctx, sess.MacaroonRootKey, &session.MacaroonRecipe{
				Permissions: GetAllPermissions(readOnly),
			},
		)
		if err != nil {
			log.Debugf("Not resuming session %x. Could not bake"+
				"the necessary macaroon: %w", pubKeyBytes, err)
			return nil
		}

		authData = []byte(fmt.Sprintf("%s: %s", HeaderMacaroon, mac))

	default:
		log.Debugf("Not resuming session %x with type %d", pubKeyBytes,
			sess.Type)
		return nil
	}

	sessionClosedSub, err := s.sessionServer.StartSession(
		sess, authData, s.db.StoreSession,
	)
	if err != nil {
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		ticker := time.NewTimer(time.Until(sess.Expiry))
		defer ticker.Stop()

		select {
		case <-s.quit:
		case <-sessionClosedSub:
		case <-ticker.C:
			log.Debugf("Stopping expired session %x with "+
				"type %d", pubKeyBytes, sess.Type)

			err = s.sessionServer.StopSession(pubKey)
			if err != nil {
				log.Debugf("Error stopping session: "+
					"%v", err)
			}

			err = s.db.RevokeSession(pubKey)
			if err != nil {
				log.Debugf("error revoking session: "+
					"%v", err)
			}
		}
	}()

	return nil
}

// ListSessions returns all sessions known to the session store.
func (s *sessionRpcServer) ListSessions(_ context.Context,
	_ *litrpc.ListSessionsRequest) (*litrpc.ListSessionsResponse, error) {

	sessions, err := s.db.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("error fetching sessions: %v", err)
	}

	response := &litrpc.ListSessionsResponse{
		Sessions: make([]*litrpc.Session, len(sessions)),
	}
	for idx, sess := range sessions {
		response.Sessions[idx], err = marshalRPCSession(sess)
		if err != nil {
			return nil, fmt.Errorf("error marshaling session: %v",
				err)
		}
	}

	return response, nil
}

// RevokeSession revokes a single session and also stops it if it is currently
// active.
func (s *sessionRpcServer) RevokeSession(_ context.Context,
	req *litrpc.RevokeSessionRequest) (*litrpc.RevokeSessionResponse, error) {

	pubKey, err := btcec.ParsePubKey(req.LocalPublicKey, btcec.S256())
	if err != nil {
		return nil, fmt.Errorf("error parsing public key: %v", err)
	}

	if err := s.db.RevokeSession(pubKey); err != nil {
		return nil, fmt.Errorf("error revoking session: %v", err)
	}

	// If the session expired already it might not be running anymore. So we
	// only log possible errors here.
	if err := s.sessionServer.StopSession(pubKey); err != nil {
		log.Debugf("Error stopping session: %v", err)
	}

	return &litrpc.RevokeSessionResponse{}, nil
}

// marshalRPCSession converts a session into its RPC counterpart.
func marshalRPCSession(sess *session.Session) (*litrpc.Session, error) {
	rpcState, err := marshalRPCState(sess.State)
	if err != nil {
		return nil, err
	}

	rpcType, err := marshalRPCType(sess.Type)
	if err != nil {
		return nil, err
	}

	var remotePubKey []byte
	if sess.RemotePublicKey != nil {
		remotePubKey = sess.RemotePublicKey.SerializeCompressed()
	}

	mnemonic, err := mailbox.PassphraseEntropyToMnemonic(sess.PairingSecret)
	if err != nil {
		return nil, err
	}

	return &litrpc.Session{
		Label:                  sess.Label,
		SessionState:           rpcState,
		SessionType:            rpcType,
		ExpiryTimestampSeconds: uint64(sess.Expiry.Unix()),
		MailboxServerAddr:      sess.ServerAddr,
		DevServer:              sess.DevServer,
		PairingSecret:          sess.PairingSecret[:],
		PairingSecretMnemonic:  strings.Join(mnemonic[:], " "),
		LocalPublicKey:         sess.LocalPublicKey.SerializeCompressed(),
		RemotePublicKey:        remotePubKey,
	}, nil
}

// marshalRPCState converts a session state to its RPC counterpart.
func marshalRPCState(state session.State) (litrpc.SessionState, error) {
	switch state {
	case session.StateCreated:
		return litrpc.SessionState_STATE_CREATED, nil

	case session.StateInUse:
		return litrpc.SessionState_STATE_IN_USE, nil

	case session.StateRevoked:
		return litrpc.SessionState_STATE_REVOKED, nil

	case session.StateExpired:
		return litrpc.SessionState_STATE_EXPIRED, nil

	default:
		return 0, fmt.Errorf("unknown state <%d>", state)
	}
}

// marshalRPCType converts a session type to its RPC counterpart.
func marshalRPCType(typ session.Type) (litrpc.SessionType, error) {
	switch typ {
	case session.TypeMacaroonReadonly:
		return litrpc.SessionType_TYPE_MACAROON_READONLY, nil

	case session.TypeMacaroonAdmin:
		return litrpc.SessionType_TYPE_MACAROON_ADMIN, nil

	case session.TypeMacaroonCustom:
		return litrpc.SessionType_TYPE_MACAROON_CUSTOM, nil

	case session.TypeUIPassword:
		return litrpc.SessionType_TYPE_UI_PASSWORD, nil

	default:
		return 0, fmt.Errorf("unknown type <%d>", typ)
	}
}

// unmarshalRPCType converts an RPC session type to its session counterpart.
func unmarshalRPCType(typ litrpc.SessionType) (session.Type, error) {
	switch typ {
	case litrpc.SessionType_TYPE_MACAROON_READONLY:
		return session.TypeMacaroonReadonly, nil

	case litrpc.SessionType_TYPE_MACAROON_ADMIN:
		return session.TypeMacaroonAdmin, nil

	case litrpc.SessionType_TYPE_MACAROON_CUSTOM:
		return session.TypeMacaroonCustom, nil

	case litrpc.SessionType_TYPE_UI_PASSWORD:
		return session.TypeUIPassword, nil

	default:
		return 0, fmt.Errorf("unknown type <%d>", typ)
	}
}
