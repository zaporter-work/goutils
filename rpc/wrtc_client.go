package rpc

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"

	"github.com/edaniels/golog"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"go.uber.org/multierr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"go.viam.com/utils"
	webrtcpb "go.viam.com/utils/proto/rpc/webrtc/v1"
)

// ErrNoWebRTCSignaler happens if a gRPC request is made on a server that does not support
// signaling for WebRTC.
var ErrNoWebRTCSignaler = errors.New("no signaler present")

// DialWebRTCOptions control how WebRTC is utilized in a dial attempt.
type DialWebRTCOptions struct {
	// Insecure determines if the WebRTC connection is DTLS based.
	Insecure bool

	// Signaling server specifies the signaling server to
	// contact on behalf of this client for WebRTC communications.
	SignalingServer string

	// DisableTrickleICE controls whether to disable Trickle ICE or not.
	// Disabling Trickle ICE can slow down connection establishment.
	DisableTrickleICE bool

	// Config is the WebRTC specific configuration (i.e. ICE settings)
	Config *webrtc.Configuration
}

// DialWebRTC connects to the signaling service at the given address and attempts to establish
// a WebRTC connection with the corresponding peer reflected in the address.
// It provider client/server functionality for gRPC serviced over
// WebRTC data channels. The work is adapted from https://github.com/jsmouret/grpc-over-webrtc.
func DialWebRTC(ctx context.Context, address string, logger golog.Logger, opts ...DialOption) (conn ClientConn, err error) {
	var dOpts dialOptions
	for _, opt := range opts {
		opt.apply(&dOpts)
	}
	return dialWebRTC(ctx, address, &dOpts, logger)
}

func dialWebRTC(ctx context.Context, address string, dOpts *dialOptions, logger golog.Logger) (ch *webrtcClientChannel, err error) {
	logger = logger.Named("webrtc")
	var host string
	if u, err := url.Parse(address); err == nil {
		address = u.Host
		host = u.Query().Get("host")
	}
	dialCtx, timeoutCancel := context.WithTimeout(ctx, webrtcConnectionTimeout)
	defer timeoutCancel()

	logger.Debugw("connecting to signaling server", "address", address)

	dOptsCopy := *dOpts
	if dOpts.webrtcOpts.Insecure {
		dOptsCopy.insecure = true
	} else {
		dOptsCopy.insecure = false
	}
	conn, _, err := dialDirectGRPC(dialCtx, address, &dOptsCopy, logger)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = multierr.Combine(err, conn.Close())
	}()

	logger.Debug("connected")

	md := metadata.New(map[string]string{RPCHostMetadataField: host})
	signalCtx := metadata.NewOutgoingContext(ctx, md)

	signalingClient := webrtcpb.NewSignalingServiceClient(conn)
	configResp, err := signalingClient.OptionalWebRTCConfig(signalCtx, &webrtcpb.OptionalWebRTCConfigRequest{})
	if err != nil {
		return nil, err
	}

	config := DefaultWebRTCConfiguration
	if dOptsCopy.webrtcOpts.Config != nil {
		config = *dOptsCopy.webrtcOpts.Config
	}
	extendedConfig := extendWebRTCConfig(&config, configResp.Config)
	pc, dc, err := newPeerConnectionForClient(ctx, extendedConfig, dOptsCopy.webrtcOpts.DisableTrickleICE, logger)
	if err != nil {
		return nil, err
	}
	var successful bool
	defer func() {
		if !successful {
			err = multierr.Combine(err, pc.Close())
		}
	}()

	exchangeCtx, exchangeCancel := context.WithCancel(signalCtx)
	defer exchangeCancel()

	errCh := make(chan error)
	sendErr := func(err error) {
		select {
		case <-exchangeCtx.Done():
		case errCh <- err:
		}
	}
	var uuid string
	// only send once since exchange may end or ICE may end
	var sendDoneErrorOnce sync.Once
	sendDone := func() error {
		var err error
		sendDoneErrorOnce.Do(func() {
			_, err = signalingClient.CallUpdate(exchangeCtx, &webrtcpb.CallUpdateRequest{
				Uuid: uuid,
				Update: &webrtcpb.CallUpdateRequest_Done{
					Done: true,
				},
			})
		})
		return err
	}

	remoteDescSet := make(chan struct{})
	if !dOptsCopy.webrtcOpts.DisableTrickleICE {
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			return nil, err
		}

		pc.OnICECandidate(func(i *webrtc.ICECandidate) {
			if exchangeCtx.Err() != nil {
				return
			}
			select {
			case <-remoteDescSet:
			case <-exchangeCtx.Done():
				return
			}
			if i == nil {
				if err := sendDone(); err != nil {
					sendErr(err)
				}
				return
			}
			iProto := iceCandidateToProto(i)
			if _, err := signalingClient.CallUpdate(exchangeCtx, &webrtcpb.CallUpdateRequest{
				Uuid: uuid,
				Update: &webrtcpb.CallUpdateRequest_Candidate{
					Candidate: iProto,
				},
			}); err != nil {
				sendErr(err)
			}
		})

		err = pc.SetLocalDescription(offer)
		if err != nil {
			return nil, err
		}
	}

	encodedSDP, err := encodeSDP(pc.LocalDescription())
	if err != nil {
		return nil, err
	}

	callClient, err := signalingClient.Call(signalCtx, &webrtcpb.CallRequest{Sdp: encodedSDP})
	if err != nil {
		if s, ok := status.FromError(err); ok && s.Code() == codes.Unimplemented {
			return nil, ErrNoWebRTCSignaler
		}
		return nil, err
	}

	// TODO(https://github.com/viamrobotics/goutils/issues/12): do separate auth here
	clientCh := newWebRTCClientChannel(pc, dc, logger)

	exchangeCandidates := func() error {
		haveInit := false
		for {
			if err := exchangeCtx.Err(); err != nil {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}

			callResp, err := callClient.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return err
				}
				return nil
			}
			switch s := callResp.Stage.(type) {
			case *webrtcpb.CallResponse_Init:
				if haveInit {
					return errors.New("got init stage more than once")
				}
				haveInit = true
				uuid = callResp.Uuid
				answer := webrtc.SessionDescription{}
				if err := decodeSDP(s.Init.Sdp, &answer); err != nil {
					return err
				}

				err = pc.SetRemoteDescription(answer)
				if err != nil {
					return err
				}
				close(remoteDescSet)

				if dOptsCopy.webrtcOpts.DisableTrickleICE {
					return sendDone()
				}
			case *webrtcpb.CallResponse_Update:
				if !haveInit {
					return errors.New("got update stage before init stage")
				}
				if callResp.Uuid != uuid {
					return errors.Errorf("uuid mismatch; have=%q want=%q", callResp.Uuid, uuid)
				}
				cand := iceCandidateFromProto(s.Update.Candidate)
				if err := pc.AddICECandidate(cand); err != nil {
					return err
				}
			default:
				return errors.Errorf("unexpected stage %T", s)
			}
		}
	}

	utils.PanicCapturingGoWithCallback(func() {
		if err := exchangeCandidates(); err != nil {
			sendErr(err)
		}
	}, func(err interface{}) {
		sendErr(fmt.Errorf("%w", err))
	})

	doCall := func() error {
		select {
		case <-ctx.Done():
			return multierr.Combine(ctx.Err(), clientCh.Close())
		case <-clientCh.Ready():
			return nil
		case err := <-errCh:
			return multierr.Combine(err, clientCh.Close())
		}
	}

	if callErr := doCall(); callErr != nil {
		var err error
		sendDoneErrorOnce.Do(func() {
			_, err = signalingClient.CallUpdate(exchangeCtx, &webrtcpb.CallUpdateRequest{
				Uuid: uuid,
				Update: &webrtcpb.CallUpdateRequest_Error{
					Error: ErrorToStatus(callErr).Proto(),
				},
			})
		})
		return nil, multierr.Combine(callErr, err)
	}
	if err := sendDone(); err != nil {
		return nil, err
	}
	successful = true
	return clientCh, nil
}