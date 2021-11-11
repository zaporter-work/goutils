package rpcwebrtc

import (
	"context"
	"io"
	"time"

	"github.com/edaniels/golog"
	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v3"
	"go.uber.org/multierr"

	"go.viam.com/utils"
	webrtcpb "go.viam.com/utils/proto/rpc/webrtc/v1"
)

const connectionTimeout = 20 * time.Second

var (
	// DefaultICEServers is the default set of ICE servers to use for WebRTC session negotiation.
	// There is no guarantee that the defaults here will remain usable.
	DefaultICEServers = []webrtc.ICEServer{
		// feel free to use your own ICE servers
		{
			URLs: []string{"stun:global.stun.twilio.com:3478?transport=udp"},
		},
	}
)

// DefaultWebRTCConfiguration is the standard configuration used for WebRTC peers.
var DefaultWebRTCConfiguration = webrtc.Configuration{
	ICEServers: DefaultICEServers,
}

func newWebRTCAPI(logger golog.Logger) (*webrtc.API, error) {
	m := webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	i := interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(&m, &i); err != nil {
		return nil, err
	}

	options := []func(a *webrtc.API){webrtc.WithMediaEngine(&m), webrtc.WithInterceptorRegistry(&i)}
	if utils.Debug {
		options = append(options, webrtc.WithSettingEngine(webrtc.SettingEngine{
			LoggerFactory: LoggerFactory{logger},
		}))
	}
	return webrtc.NewAPI(options...), nil
}

func newPeerConnectionForClient(ctx context.Context, config webrtc.Configuration, disableTrickle bool, logger golog.Logger) (pc *webrtc.PeerConnection, dc *webrtc.DataChannel, err error) {
	webAPI, err := newWebRTCAPI(logger)
	if err != nil {
		return nil, nil, err
	}

	pc, err = webAPI.NewPeerConnection(config)
	if err != nil {
		return nil, nil, err
	}
	var successful bool
	defer func() {
		if !successful {
			err = multierr.Combine(err, pc.Close())
		}
	}()

	negotiated := true
	ordered := true
	dataChannelID := uint16(0)
	dataChannel, err := pc.CreateDataChannel("data", &webrtc.DataChannelInit{
		ID:         &dataChannelID,
		Negotiated: &negotiated,
		Ordered:    &ordered,
	})
	if err != nil {
		return pc, nil, err
	}
	dataChannel.OnError(initialDataChannelOnError(pc, logger))

	if disableTrickle {
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			return pc, nil, err
		}

		// Sets the LocalDescription, and starts our UDP listeners
		err = pc.SetLocalDescription(offer)
		if err != nil {
			return pc, nil, err
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(pc)

		// Block until ICE Gathering is complete since we signal back one complete SDP
		// and do not want to wait on trickle ICE.
		select {
		case <-ctx.Done():
			return pc, nil, ctx.Err()
		case <-gatherComplete:
		}
	}

	// Will not wait for connection to establish. If you want this in the future,
	// add a state check to OnICEConnectionStateChange for webrtc.ICEConnectionStateConnected.
	successful = true
	return pc, dataChannel, nil
}

func newPeerConnectionForServer(ctx context.Context, sdp string, config webrtc.Configuration, disableTrickle bool, logger golog.Logger) (pc *webrtc.PeerConnection, dc *webrtc.DataChannel, err error) {
	webAPI, err := newWebRTCAPI(logger)
	if err != nil {
		return nil, nil, err
	}

	pc, err = webAPI.NewPeerConnection(config)
	if err != nil {
		return nil, nil, err
	}
	var successful bool
	defer func() {
		if !successful {
			err = multierr.Combine(err, pc.Close())
		}
	}()

	negotiated := true
	ordered := true
	dataChannelID := uint16(0)
	dataChannel, err := pc.CreateDataChannel("data", &webrtc.DataChannelInit{
		ID:         &dataChannelID,
		Negotiated: &negotiated,
		Ordered:    &ordered,
	})
	if err != nil {
		return pc, dataChannel, err
	}
	dataChannel.OnError(initialDataChannelOnError(pc, logger))

	offer := webrtc.SessionDescription{}
	if err := DecodeSDP(sdp, &offer); err != nil {
		return pc, dataChannel, err
	}

	err = pc.SetRemoteDescription(offer)
	if err != nil {
		return pc, dataChannel, err
	}

	if disableTrickle {
		answer, err := pc.CreateAnswer(nil)
		if err != nil {
			return pc, dataChannel, err
		}

		err = pc.SetLocalDescription(answer)
		if err != nil {
			return pc, dataChannel, err
		}

		// Create channel that is blocked until ICE Gathering is complete
		gatherComplete := webrtc.GatheringCompletePromise(pc)

		// Block until ICE Gathering is complete since we signal back one complete SDP
		// and do not want to wait on trickle ICE.
		select {
		case <-ctx.Done():
			return pc, nil, ctx.Err()
		case <-gatherComplete:
		}
	}

	successful = true
	return pc, dataChannel, nil
}

type peerConnectionStats struct {
	ID               string
	RemoteCandidates map[string]string
}

func getPeerConnectionStats(peerConnection *webrtc.PeerConnection) peerConnectionStats {
	stats := peerConnection.GetStats()
	var connID string
	connInfo := map[string]string{}
	for _, stat := range stats {
		if pcStats, ok := stat.(webrtc.PeerConnectionStats); ok {
			connID = pcStats.ID
		}
		candidateStats, ok := stat.(webrtc.ICECandidateStats)
		if !ok {
			continue
		}
		if candidateStats.Type != webrtc.StatsTypeRemoteCandidate {
			continue
		}
		var candidateType string
		switch candidateStats.CandidateType {
		case webrtc.ICECandidateTypeRelay:
			candidateType = "relay"
		case webrtc.ICECandidateTypePrflx:
			candidateType = "peer-reflexive"
		case webrtc.ICECandidateTypeSrflx:
			candidateType = "server-reflexive"
		}
		if candidateType == "" {
			continue
		}
		connInfo[candidateType] = candidateStats.IP
	}
	return peerConnectionStats{connID, connInfo}
}

func initialDataChannelOnError(pc io.Closer, logger golog.Logger) func(err error) {
	return func(err error) {
		logger.Errorw("premature data channel error before WebRTC channel association", "error", err)
		utils.UncheckedError(pc.Close())
	}
}

func iceCandidateToProto(i *webrtc.ICECandidate) *webrtcpb.ICECandidate {
	return iceCandidateInitToProto(i.ToJSON())
}

func iceCandidateInitToProto(ij webrtc.ICECandidateInit) *webrtcpb.ICECandidate {
	candidate := webrtcpb.ICECandidate{
		Candidate: ij.Candidate,
	}
	if ij.SDPMid != nil {
		val := *ij.SDPMid
		candidate.SdpMid = &val
	}
	if ij.SDPMLineIndex != nil {
		val := uint32(*ij.SDPMLineIndex)
		candidate.SdpmLineIndex = &val
	}
	if ij.UsernameFragment != nil {
		val := *ij.UsernameFragment
		candidate.UsernameFragment = &val
	}
	return &candidate
}

func iceCandidateFromProto(i *webrtcpb.ICECandidate) webrtc.ICECandidateInit {
	candidate := webrtc.ICECandidateInit{
		Candidate: i.Candidate,
	}
	if i.SdpMid != nil {
		val := *i.SdpMid
		candidate.SDPMid = &val
	}
	if i.SdpmLineIndex != nil {
		val := uint16(*i.SdpmLineIndex)
		candidate.SDPMLineIndex = &val
	}
	if i.UsernameFragment != nil {
		val := *i.UsernameFragment
		candidate.UsernameFragment = &val
	}
	return candidate
}
