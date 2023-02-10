package rtc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v2"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/config"
	logging "github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	dd "github.com/livekit/livekit-server/pkg/sfu/dependencydescriptor"
	"github.com/livekit/protocol/logger"
)

const (
	minUDPBufferSize     = 5_000_000
	defaultUDPBufferSize = 16_777_216
	frameMarking         = "urn:ietf:params:rtp-hdrext:framemarking"
)

type WebRTCConfig struct {
	Configuration  webrtc.Configuration
	SettingEngine  webrtc.SettingEngine
	Receiver       ReceiverConfig
	BufferFactory  *buffer.Factory
	UDPMux         ice.UDPMux
	TCPMuxListener *net.TCPListener
	Publisher      DirectionConfig
	Subscriber     DirectionConfig
	NAT1To1IPs     []string
	UseMDNS        bool
}

type ReceiverConfig struct {
	PacketBufferSize int
}

type RTPHeaderExtensionConfig struct {
	Audio []string
	Video []string
}

type RTCPFeedbackConfig struct {
	Audio []webrtc.RTCPFeedback
	Video []webrtc.RTCPFeedback
}

type DirectionConfig struct {
	RTPHeaderExtension RTPHeaderExtensionConfig
	RTCPFeedback       RTCPFeedbackConfig
	StrictACKs         bool
}

const (
	// number of packets to buffer up
	readBufferSize = 50

	writeBufferSizeInBytes = 4 * 1024 * 1024
)

func NewWebRTCConfig(conf *config.Config, externalIP string) (*WebRTCConfig, error) {
	rtcConf := conf.RTC
	c := webrtc.Configuration{
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlan,
	}
	s := webrtc.SettingEngine{
		LoggerFactory: logging.NewLoggerFactory(logger.GetLogger()),
	}

	var ifFilter func(string) bool
	if len(rtcConf.Interfaces.Includes) != 0 || len(rtcConf.Interfaces.Excludes) != 0 {
		ifFilter = InterfaceFilterFromConf(rtcConf.Interfaces)
		s.SetInterfaceFilter(ifFilter)
	}

	var ipFilter func(net.IP) bool
	if len(rtcConf.IPs.Includes) != 0 || len(rtcConf.IPs.Excludes) != 0 {
		filter, err := IPFilterFromConf(rtcConf.IPs)
		if err != nil {
			return nil, err
		}
		ipFilter = filter
		s.SetIPFilter(filter)
	}

	if !rtcConf.UseMDNS {
		s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	}

	var nat1to1IPs []string
	// force it to the node IPs that the user has set
	if externalIP != "" && (conf.RTC.UseExternalIP || (conf.RTC.NodeIP != "" && !conf.RTC.NodeIPAutoGenerated)) {
		if conf.RTC.UseExternalIP {
			ips, err := getNAT1to1IPsForConf(conf, ipFilter)
			if err != nil {
				return nil, err
			}
			if len(ips) == 0 {
				logger.Infow("no external IPs found, using node IP for NAT1To1Ips", "ip", externalIP)
				s.SetNAT1To1IPs([]string{externalIP}, webrtc.ICECandidateTypeHost)
			} else {
				logger.Infow("using external IPs", "ips", ips)
				s.SetNAT1To1IPs(ips, webrtc.ICECandidateTypeHost)
			}
			nat1to1IPs = ips
		} else {
			s.SetNAT1To1IPs([]string{externalIP}, webrtc.ICECandidateTypeHost)
		}
	}

	if rtcConf.PacketBufferSize == 0 {
		rtcConf.PacketBufferSize = 500
	}

	var udpMux ice.UDPMux
	var err error
	networkTypes := make([]webrtc.NetworkType, 0, 4)

	if !rtcConf.ForceTCP {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6,
		)
		if rtcConf.ICEPortRangeStart != 0 && rtcConf.ICEPortRangeEnd != 0 {
			if err := s.SetEphemeralUDPPortRange(uint16(rtcConf.ICEPortRangeStart), uint16(rtcConf.ICEPortRangeEnd)); err != nil {
				return nil, err
			}
		} else if rtcConf.UDPPort != 0 {
			opts := []ice.UDPMuxFromPortOption{
				ice.UDPMuxFromPortWithReadBufferSize(defaultUDPBufferSize),
				ice.UDPMuxFromPortWithWriteBufferSize(defaultUDPBufferSize),
				ice.UDPMuxFromPortWithLogger(s.LoggerFactory.NewLogger("udp_mux")),
			}
			if rtcConf.EnableLoopbackCandidate {
				opts = append(opts, ice.UDPMuxFromPortWithLoopback())
			}
			if ipFilter != nil {
				opts = append(opts, ice.UDPMuxFromPortWithIPFilter(ipFilter))
			}
			if ifFilter != nil {
				opts = append(opts, ice.UDPMuxFromPortWithInterfaceFilter(ifFilter))
			}
			udpMux, err := ice.NewMultiUDPMuxFromPort(int(rtcConf.UDPPort), opts...)
			if err != nil {
				return nil, err
			}

			s.SetICEUDPMux(udpMux)
			if !conf.Development {
				checkUDPReadBuffer()
			}
		}
	}

	// use TCP mux when it's set
	var tcpListener *net.TCPListener
	if rtcConf.TCPPort != 0 {
		networkTypes = append(networkTypes,
			webrtc.NetworkTypeTCP4, webrtc.NetworkTypeTCP6,
		)
		tcpListener, err = net.ListenTCP("tcp", &net.TCPAddr{
			Port: int(rtcConf.TCPPort),
		})
		if err != nil {
			return nil, err
		}

		tcpMux := ice.NewTCPMuxDefault(ice.TCPMuxParams{
			Logger:          s.LoggerFactory.NewLogger("tcp_mux"),
			Listener:        tcpListener,
			ReadBufferSize:  readBufferSize,
			WriteBufferSize: writeBufferSizeInBytes,
		})

		s.SetICETCPMux(tcpMux)
	}

	if len(networkTypes) == 0 {
		return nil, errors.New("TCP is forced but not configured")
	}
	s.SetNetworkTypes(networkTypes)

	if rtcConf.EnableLoopbackCandidate {
		s.SetIncludeLoopbackCandidate(true)
	}

	// publisher configuration
	publisherConfig := DirectionConfig{
		StrictACKs: true, // publisher is dialed, and will always reply with ACK
		RTPHeaderExtension: RTPHeaderExtensionConfig{
			Audio: []string{
				sdp.SDESMidURI,
				sdp.SDESRTPStreamIDURI,
				sdp.AudioLevelURI,
			},
			Video: []string{
				sdp.SDESMidURI,
				sdp.SDESRTPStreamIDURI,
				sdp.TransportCCURI,
				frameMarking,
				dd.ExtensionUrl,
			},
		},
		RTCPFeedback: RTCPFeedbackConfig{
			Audio: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBNACK},
			},
			Video: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBTransportCC},
				{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
				{Type: webrtc.TypeRTCPFBNACK},
				{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
	}

	// subscriber configuration
	subscriberConfig := DirectionConfig{
		StrictACKs: conf.RTC.StrictACKs,
		RTPHeaderExtension: RTPHeaderExtensionConfig{
			Video: []string{dd.ExtensionUrl},
		},
		RTCPFeedback: RTCPFeedbackConfig{
			Video: []webrtc.RTCPFeedback{
				{Type: webrtc.TypeRTCPFBCCM, Parameter: "fir"},
				{Type: webrtc.TypeRTCPFBNACK},
				{Type: webrtc.TypeRTCPFBNACK, Parameter: "pli"},
			},
		},
	}
	if rtcConf.CongestionControl.UseSendSideBWE {
		subscriberConfig.RTPHeaderExtension.Video = append(subscriberConfig.RTPHeaderExtension.Video, sdp.TransportCCURI)
		subscriberConfig.RTCPFeedback.Video = append(subscriberConfig.RTCPFeedback.Video, webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBTransportCC})
	} else {
		subscriberConfig.RTPHeaderExtension.Video = append(subscriberConfig.RTPHeaderExtension.Video, sdp.ABSSendTimeURI)
		subscriberConfig.RTCPFeedback.Video = append(subscriberConfig.RTCPFeedback.Video, webrtc.RTCPFeedback{Type: webrtc.TypeRTCPFBGoogREMB})
	}

	if rtcConf.UseICELite {
		s.SetLite(true)
	} else if rtcConf.NodeIP == "" && !rtcConf.UseExternalIP {
		// use STUN servers for server to support NAT
		// when deployed in production, we expect UseExternalIP to be used, and ports accessible
		// this is not compatible with ICE Lite
		// Do not automatically add STUN servers if nodeIP is set
		if len(rtcConf.STUNServers) > 0 {
			c.ICEServers = []webrtc.ICEServer{iceServerForStunServers(rtcConf.STUNServers)}
		} else {
			c.ICEServers = []webrtc.ICEServer{iceServerForStunServers(config.DefaultStunServers)}
		}
	}

	return &WebRTCConfig{
		Configuration: c,
		SettingEngine: s,
		Receiver: ReceiverConfig{
			PacketBufferSize: rtcConf.PacketBufferSize,
		},
		UDPMux:         udpMux,
		TCPMuxListener: tcpListener,
		Publisher:      publisherConfig,
		Subscriber:     subscriberConfig,
		NAT1To1IPs:     nat1to1IPs,
		UseMDNS:        rtcConf.UseMDNS,
	}, nil
}

func (c *WebRTCConfig) SetBufferFactory(factory *buffer.Factory) {
	c.BufferFactory = factory
	c.SettingEngine.BufferFactory = factory.GetOrNew
}

func iceServerForStunServers(servers []string) webrtc.ICEServer {
	iceServer := webrtc.ICEServer{}
	for _, stunServer := range servers {
		iceServer.URLs = append(iceServer.URLs, fmt.Sprintf("stun:%s", stunServer))
	}
	return iceServer
}

func getNAT1to1IPsForConf(conf *config.Config, ipFilter func(net.IP) bool) ([]string, error) {
	stunServers := conf.RTC.STUNServers
	if len(stunServers) == 0 {
		stunServers = config.DefaultStunServers
	}
	localIPs, err := config.GetLocalIPAddresses(conf.RTC.EnableLoopbackCandidate)
	if err != nil {
		return nil, err
	}
	type ipmapping struct {
		externalIP string
		localIP    string
	}
	addrCh := make(chan ipmapping, len(localIPs))

	var udpPorts []int
	if conf.RTC.ICEPortRangeStart != 0 && conf.RTC.ICEPortRangeEnd != 0 {
		portRangeStart, portRangeEnd := uint16(conf.RTC.ICEPortRangeStart), uint16(conf.RTC.ICEPortRangeEnd)
		for i := 0; i < 5; i++ {
			udpPorts = append(udpPorts, rand.Intn(int(portRangeEnd-portRangeStart))+int(portRangeStart))
		}
	} else if conf.RTC.UDPPort != 0 {
		udpPorts = append(udpPorts, int(conf.RTC.UDPPort))
	} else {
		udpPorts = append(udpPorts, 0)
	}

	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	for _, ip := range localIPs {
		if ipFilter != nil && !ipFilter(net.ParseIP(ip)) {
			continue
		}

		wg.Add(1)
		go func(localIP string) {
			defer wg.Done()
			for _, port := range udpPorts {
				addr, err := config.GetExternalIP(ctx, stunServers, &net.UDPAddr{IP: net.ParseIP(localIP), Port: port})
				if err != nil {
					if strings.Contains(err.Error(), "address already in use") {
						logger.Debugw("failed to get external ip, address already in use", "local", localIP, "port", port)
						continue
					}
					logger.Infow("failed to get external ip", "local", localIP, "err", err)
					return
				}
				addrCh <- ipmapping{externalIP: addr, localIP: localIP}
				return
			}
			logger.Infow("failed to get external ip after all ports tried", "local", localIP, "ports", udpPorts)
		}(ip)
	}

	var firstResloved bool
	natMapping := make(map[string]string)
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

done:
	for {
		select {
		case mapping := <-addrCh:
			if !firstResloved {
				firstResloved = true
				timeout.Reset(1 * time.Second)
			}
			if local, ok := natMapping[mapping.externalIP]; ok {
				logger.Infow("external ip already solved, ignore duplicate",
					"external", mapping.externalIP,
					"local", local,
					"ignore", mapping.localIP)
			} else {
				natMapping[mapping.externalIP] = mapping.localIP
			}

		case <-timeout.C:
			break done
		}
	}
	cancel()
	wg.Wait()

	if len(natMapping) == 0 {
		// no external ip resolved
		return nil, nil
	}

	// mapping unresolved local ip to itself
	for _, local := range localIPs {
		var found bool
		for _, localIPMapping := range natMapping {
			if local == localIPMapping {
				found = true
				break
			}
		}
		if !found {
			natMapping[local] = local
		}
	}

	nat1to1IPs := make([]string, 0, len(natMapping))
	for external, local := range natMapping {
		nat1to1IPs = append(nat1to1IPs, fmt.Sprintf("%s/%s", external, local))
	}
	return nat1to1IPs, nil
}

func InterfaceFilterFromConf(ifs config.InterfacesConfig) func(string) bool {
	includes := ifs.Includes
	excludes := ifs.Excludes
	return func(s string) bool {
		// filter by include interfaces
		if len(includes) > 0 {
			for _, iface := range includes {
				if iface == s {
					return true
				}
			}
			return false
		}

		// filter by exclude interfaces
		if len(excludes) > 0 {
			for _, iface := range excludes {
				if iface == s {
					return false
				}
			}
		}
		return true
	}
}

func IPFilterFromConf(ips config.IPsConfig) (func(ip net.IP) bool, error) {
	var ipnets [2][]*net.IPNet
	var err error
	for i, ips := range [][]string{ips.Includes, ips.Excludes} {
		ipnets[i], err = func(fromIPs []string) ([]*net.IPNet, error) {
			var toNets []*net.IPNet
			for _, ip := range fromIPs {
				_, ipnet, err := net.ParseCIDR(ip)
				if err != nil {
					return nil, err
				}
				toNets = append(toNets, ipnet)
			}
			return toNets, nil
		}(ips)

		if err != nil {
			return nil, err
		}
	}

	includes, excludes := ipnets[0], ipnets[1]

	return func(ip net.IP) bool {
		if len(includes) > 0 {
			for _, ipn := range includes {
				if ipn.Contains(ip) {
					return true
				}
			}
			return false
		}

		if len(excludes) > 0 {
			for _, ipn := range excludes {
				if ipn.Contains(ip) {
					return false
				}
			}
		}
		return true
	}, nil
}
