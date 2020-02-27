package pcap

import (
	"errors"
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"net"
)

// Server describes the packet capture on the server side
type Server struct {
	ListenPort    uint16
	ListenDevs    []*Device
	UpDev         *Device
	GatewayDev    *Device
	listenHandles []*pcap.Handle
	upHandle      *pcap.Handle
	seqs          map[string]uint32
	acks          map[string]uint32
	// TODO: attempt to initialize IPv4 id to reduce the possibility of collision
	id       uint16
	port     uint16
	portDist map[quintuple]uint16
	nat      map[quintuple]encappedPacketSrc
}

// Open implements a method opens the pcap
func (p *Server) Open() error {
	p.seqs = make(map[string]uint32)
	p.acks = make(map[string]uint32)
	p.id = 0
	p.portDist = make(map[quintuple]uint16)
	p.nat = make(map[quintuple]encappedPacketSrc)

	// Verify
	if len(p.ListenDevs) <= 0 {
		return fmt.Errorf("open: %w", errors.New("missing listen device"))
	}
	if p.UpDev == nil {
		return fmt.Errorf("open: %w", errors.New("missing upstream device"))
	}
	if p.GatewayDev == nil {
		return fmt.Errorf("open: %w", errors.New("missing gateway"))
	}
	if len(p.ListenDevs) == 1 {
		dev := p.ListenDevs[0]
		strIPs := ""
		for i, addr := range dev.IPAddrs {
			if i != 0 {
				strIPs = strIPs + fmt.Sprintf(", %s", addr.IP)
			} else {
				strIPs = strIPs + addr.IP.String()
			}
		}
		if dev.IsLoop {
			fmt.Printf("Listen on %s: %s\n", dev.FriendlyName, strIPs)
		} else {
			fmt.Printf("Listen on %s [%s]: %s\n", dev.FriendlyName, dev.HardwareAddr, strIPs)
		}
	} else {
		fmt.Println("Listen on:")
		for _, dev := range p.ListenDevs {
			strIPs := ""
			for j, addr := range dev.IPAddrs {
				if j != 0 {
					strIPs = strIPs + fmt.Sprintf(", %s", addr.IP)
				} else {
					strIPs = strIPs + addr.IP.String()
				}
			}
			if dev.IsLoop {
				fmt.Printf("  %s: %s\n", dev.FriendlyName, strIPs)
			} else {
				fmt.Printf("  %s [%s]: %s\n", dev.FriendlyName, dev.HardwareAddr, strIPs)
			}
		}
	}
	strUpIPs := ""
	for i, addr := range p.UpDev.IPAddrs {
		if i != 0 {
			strUpIPs = strUpIPs + fmt.Sprintf(", %s", addr.IP)
		} else {
			strUpIPs = strUpIPs + addr.IP.String()
		}
	}
	if !p.GatewayDev.IsLoop {
		fmt.Printf("Route upstream from %s [%s]: %s to gateway [%s]: %s\n", p.UpDev.FriendlyName,
			p.UpDev.HardwareAddr, strUpIPs, p.GatewayDev.HardwareAddr, p.GatewayDev.IPAddr().IP)
	} else {
		fmt.Printf("Route upstream to loopback %s\n", p.UpDev.FriendlyName)
	}

	// Handles for listening
	p.listenHandles = make([]*pcap.Handle, 0)
	for _, dev := range p.ListenDevs {
		handle, err := pcap.OpenLive(dev.Name, 1600, true, pcap.BlockForever)
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		err = handle.SetBPFFilter(fmt.Sprintf("tcp && dst port %d", p.ListenPort))
		if err != nil {
			return fmt.Errorf("open: %w", err)
		}
		p.listenHandles = append(p.listenHandles, handle)
	}

	// Handles for routing upstream
	var err error
	p.upHandle, err = pcap.OpenLive(p.UpDev.Name, 1600, true, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	err = p.upHandle.SetBPFFilter(fmt.Sprintf("(tcp || udp) && not dst port %d", p.ListenPort))
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	// Start handling
	for _, handle := range p.listenHandles {
		packetSrc := gopacket.NewPacketSource(handle, handle.LinkType())
		go func() {
			for packet := range packetSrc.Packets() {
				p.handleListen(packet, handle)
			}
		}()
	}
	packetSrc := gopacket.NewPacketSource(p.upHandle, p.upHandle.LinkType())
	for packet := range packetSrc.Packets() {
		p.handleUpstream(packet)
	}

	return nil
}

// Close implements a method closes the pcap
func (p *Server) Close() {
	for _, handle := range p.listenHandles {
		handle.Close()
	}
	p.upHandle.Close()
}

func (p *Server) handshake(indicator *packetIndicator) error {
	var (
		newTransportLayer   *layers.TCP
		newNetworkLayerType gopacket.LayerType
		newNetworkLayer     gopacket.NetworkLayer
		newLinkLayerType    gopacket.LayerType
		newLinkLayer        gopacket.Layer
	)

	// Initial TCP Seq
	srcAddr := indicator.SrcAddr()
	p.seqs[srcAddr] = 0

	// TCK Ack
	p.acks[srcAddr] = indicator.Seq + 1

	// Create transport layer
	newTransportLayer = createTCPLayerSYNACK(p.ListenPort, indicator.SrcPort, p.seqs[srcAddr], p.acks[srcAddr])

	// Decide IPv4 or IPv6
	if indicator.DstIP.To4() != nil {
		newNetworkLayerType = layers.LayerTypeIPv4
	} else {
		newNetworkLayerType = layers.LayerTypeIPv6
	}

	// Create new network layer
	var err error
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		newNetworkLayer, err = createNetworkLayerIPv4(indicator.DstIP,
			indicator.SrcIP, p.id, 128, newTransportLayer)
	case layers.LayerTypeIPv6:
		newNetworkLayer, err = createNetworkLayerIPv6(indicator.DstIP, indicator.SrcIP, newTransportLayer)
	default:
		return fmt.Errorf("handshake: %w",
			fmt.Errorf("create network layer: %w",
				fmt.Errorf("type %s not support", newNetworkLayerType)))
	}
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	// Decide Loopback or Ethernet
	if p.UpDev.IsLoop {
		newLinkLayerType = layers.LayerTypeLoopback
	} else {
		newLinkLayerType = layers.LayerTypeEthernet
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = createLinkLayerLoopback()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = createLinkLayerEthernet(p.UpDev.HardwareAddr, p.GatewayDev.HardwareAddr, newNetworkLayer)
	default:
		return fmt.Errorf("handshake: %w",
			fmt.Errorf("create link layer: %w", fmt.Errorf("type %s not support", newLinkLayerType)))
	}
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	// Serialize layers
	data, err := serialize(newLinkLayer, newNetworkLayer, newTransportLayer, nil)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	// Write packet data
	err = p.upHandle.WritePacketData(data)
	if err != nil {
		return fmt.Errorf("handshake: %w", fmt.Errorf("write: %w", err))
	}

	// IPv4 Id
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		p.id++
	default:
		break
	}

	return nil
}

func (p *Server) handleListen(packet gopacket.Packet, handle *pcap.Handle) {
	var (
		indicator           *packetIndicator
		encappedIndicator   *packetIndicator
		newNetworkLayerType gopacket.LayerType
		newNetworkLayer     gopacket.NetworkLayer
		newLinkLayerType    gopacket.LayerType
		newLinkLayer        gopacket.Layer
	)

	// Parse packet
	indicator, err := parsePacket(packet)
	if err != nil {
		fmt.Println(fmt.Errorf("handle listen: %w", err))
		return
	}

	// Handshaking with client (SYN+ACK)
	if indicator.SYN {
		err := p.handshake(indicator)
		if err != nil {
			fmt.Println(fmt.Errorf("handle listen: %w", err))
			return
		}
		fmt.Printf("Connect from client %s:%d\n", indicator.SrcIP, indicator.SrcPort)
		return
	}

	// Empty payload
	if indicator.ApplicationLayer == nil {
		return
	}

	// Ack
	srcAddr := indicator.SrcAddr()
	p.acks[srcAddr] = p.acks[srcAddr] + uint32(len(indicator.ApplicationLayer.LayerContents()))

	// Parse encapped packet
	encappedIndicator, err = parseEncappedPacket(indicator.ApplicationLayer.LayerContents())
	if err != nil {
		fmt.Println(fmt.Errorf("handle listen: %w", err))
		return
	}

	// Distribute port
	qPortDist := quintuple{
		SrcIP:    encappedIndicator.SrcIP.String(),
		SrcPort:  encappedIndicator.SrcPort,
		DstIP:    indicator.SrcIP.String(),
		DstPort:  indicator.SrcPort,
		Protocol: encappedIndicator.TransportLayerType,
	}
	distPort, ok := p.portDist[qPortDist]
	if !ok {
		distPort = p.distPort()
		p.port++
	}

	// Modify transport layer
	switch encappedIndicator.TransportLayerType {
	case layers.LayerTypeTCP:
		tcpLayer := encappedIndicator.TransportLayer.(*layers.TCP)
		tcpLayer.SrcPort = layers.TCPPort(distPort)
	case layers.LayerTypeUDP:
		udpLayer := encappedIndicator.TransportLayer.(*layers.UDP)
		udpLayer.SrcPort = layers.UDPPort(distPort)
	default:
		fmt.Println(fmt.Errorf("handle listen: %w",
			fmt.Errorf("create transport layer: %w",
				fmt.Errorf("type %s not support", encappedIndicator.TransportLayerType))))
		return
	}

	// Create new network layer
	newNetworkLayerType = encappedIndicator.NetworkLayerType
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		newNetworkLayer, err = createNetworkLayerIPv4(p.UpDev.IPv4Addr().IP,
			encappedIndicator.DstIP, encappedIndicator.Id, encappedIndicator.TTL-1, encappedIndicator.TransportLayer)
	case layers.LayerTypeIPv6:
		newNetworkLayer, err = createNetworkLayerIPv6(p.UpDev.IPv6Addr().IP,
			encappedIndicator.DstIP, encappedIndicator.TransportLayer)
	default:
		fmt.Println(fmt.Errorf("handle listen: %w",
			fmt.Errorf("create network layer: %w",
				fmt.Errorf("type %s not support", newNetworkLayerType))))
		return
	}
	if err != nil {
		fmt.Println(fmt.Errorf("handle listen: %w", err))
		return
	}

	// Decide Loopback or Ethernet
	if p.UpDev.IsLoop {
		newLinkLayerType = layers.LayerTypeLoopback
	} else {
		newLinkLayerType = layers.LayerTypeEthernet
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = createLinkLayerLoopback()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = createLinkLayerEthernet(p.UpDev.HardwareAddr,
			p.GatewayDev.HardwareAddr, newNetworkLayer)
	default:
		fmt.Println(fmt.Errorf("handle listen: %w",
			fmt.Errorf("create link layer: %w", fmt.Errorf("type %s not support", newLinkLayerType))))
		return
	}
	if err != nil {
		fmt.Println(fmt.Errorf("handle listen: %w", err))
		return
	}

	// Record the source and the source device of the packet
	qNAT := quintuple{
		SrcIP:    p.UpDev.IPv4Addr().IP.String(),
		SrcPort:  encappedIndicator.SrcPort,
		DstIP:    encappedIndicator.DstIP.String(),
		DstPort:  encappedIndicator.DstPort,
		Protocol: encappedIndicator.TransportLayerType,
	}
	ps := encappedPacketSrc{
		SrcIP:           indicator.SrcIP.String(),
		SrcPort:         indicator.SrcPort,
		EncappedSrcIP:   qPortDist.SrcIP,
		EncappedSrcPort: qPortDist.SrcPort,
		Handle:          handle,
	}
	p.nat[qNAT] = ps

	// Serialize layers
	data, err := serialize(newLinkLayer, newNetworkLayer, encappedIndicator.TransportLayer, encappedIndicator.Payload())
	if err != nil {
		fmt.Println(fmt.Errorf("handle listen: %w", err))
		return
	}

	// Write packet data
	err = p.upHandle.WritePacketData(data)
	if err != nil {
		fmt.Println(fmt.Errorf("handle listen: %w", fmt.Errorf("write: %w", err)))
	}
	fmt.Printf("Redirect an inbound %s packet: %s -> %s (%d Bytes)\n",
		encappedIndicator.TransportLayerType,
		encappedIndicator.SrcAddr(), encappedIndicator.DstAddr(), packet.Metadata().Length)
}

func (p *Server) handleUpstream(packet gopacket.Packet) {
	var (
		indicator           *packetIndicator
		newTransportLayer   *layers.TCP
		upDevIP             net.IP
		newNetworkLayerType gopacket.LayerType
		newNetworkLayer     gopacket.NetworkLayer
		newLinkLayerType    gopacket.LayerType
		newLinkLayer        gopacket.Layer
	)

	// Parse packet
	indicator, err := parsePacket(packet)
	if err != nil {
		fmt.Println(fmt.Errorf("handle upstream: %w", err))
		return
	}

	// NAT
	q := quintuple{
		SrcIP:    indicator.DstIP.String(),
		SrcPort:  indicator.DstPort,
		DstIP:    indicator.SrcIP.String(),
		DstPort:  indicator.SrcPort,
		Protocol: indicator.TransportLayerType,
	}
	ps, ok := p.nat[q]
	if !ok {
		return
	}

	// NAT back encapped transport layer
	switch indicator.TransportLayerType {
	case layers.LayerTypeTCP:
		tcpLayer := indicator.TransportLayer.(*layers.TCP)
		tcpLayer.SrcPort = layers.TCPPort(ps.EncappedSrcPort)
	case layers.LayerTypeUDP:
		udpLayer := indicator.TransportLayer.(*layers.UDP)
		udpLayer.SrcPort = layers.UDPPort(ps.EncappedSrcPort)
	default:
		fmt.Println(fmt.Errorf("handle upstream: %w",
			fmt.Errorf("create encapped transport layer: %w",
				fmt.Errorf("type %s not support", indicator.TransportLayerType))))
		return
	}

	// NAT back encapped network layer
	switch indicator.NetworkLayerType {
	case layers.LayerTypeIPv4:
		ipv4Layer := indicator.NetworkLayer.(*layers.IPv4)
		ipv4Layer.SrcIP = net.ParseIP(ps.EncappedSrcIP)
	case layers.LayerTypeIPv6:
		ipv6Layer := indicator.NetworkLayer.(*layers.IPv6)
		ipv6Layer.SrcIP = net.ParseIP(ps.EncappedSrcIP)
	default:
		fmt.Println(fmt.Errorf("handle upstream: %w",
			fmt.Errorf("create encapped network layer: %w",
				fmt.Errorf("type %s not support", indicator.NetworkLayerType))))
	}

	// Construct contents of new application layer
	contents := indicator.Contents()

	// Create new transport layer
	addr := fmt.Sprintf("%s:%d", ps.SrcIP, ps.SrcPort)
	newTransportLayer = createTransportLayerTCP(p.ListenPort, ps.SrcPort, p.seqs[addr], p.acks[addr])

	// Decide IPv4 or IPv6
	isIPv4 := p.GatewayDev.IPAddr().IP.To4() != nil
	if isIPv4 {
		upDevIP = p.UpDev.IPv4Addr().IP
		newNetworkLayerType = layers.LayerTypeIPv4
	} else {
		upDevIP = p.UpDev.IPv6Addr().IP
		newNetworkLayerType = layers.LayerTypeIPv6
	}
	if upDevIP == nil {
		fmt.Println(fmt.Errorf("handle upstream: %w", errors.New("ip version transition not support")))
		return
	}

	// Create new network layer
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		newNetworkLayer, err = createNetworkLayerIPv4(upDevIP,
			net.ParseIP(ps.SrcIP), p.id, indicator.TTL-1, newTransportLayer)
	case layers.LayerTypeIPv6:
		newNetworkLayer, err = createNetworkLayerIPv6(upDevIP, net.ParseIP(ps.SrcIP), newTransportLayer)
	default:
		fmt.Println(fmt.Errorf("handle upstream: %w",
			fmt.Errorf("create network layer: %w",
				fmt.Errorf("type %s not support", newNetworkLayerType))))
		return
	}
	if err != nil {
		fmt.Println(fmt.Errorf("handle upstream: %w", err))
		return
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = createLinkLayerLoopback()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = createLinkLayerEthernet(p.UpDev.HardwareAddr, p.GatewayDev.HardwareAddr, newNetworkLayer)
	default:
		fmt.Println(fmt.Errorf("handle upstream: %w",
			fmt.Errorf("create link layer: %w", fmt.Errorf("type %s not support", newLinkLayerType))))
		return
	}
	if err != nil {
		fmt.Println(fmt.Errorf("handle upstream: %w", err))
		return
	}

	// Serialize layers
	data, err := serialize(newLinkLayer, newNetworkLayer, newTransportLayer, contents)
	if err != nil {
		fmt.Println(fmt.Errorf("handle upstream: %w", err))
		return
	}

	// Write packet data
	err = ps.Handle.WritePacketData(data)
	if err != nil {
		fmt.Println(fmt.Errorf("handle upstream: %w", fmt.Errorf("write: %w", err)))
		return
	}

	// TCP Seq
	p.seqs[addr] = p.seqs[addr] + uint32(len(contents))

	// IPv4 Id
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		p.id++
	default:
		break
	}

	fmt.Printf("Redirect an outbound %s packet: %s <- %s (%d Bytes)\n",
		indicator.TransportLayerType, indicator.SrcAddr(), indicator.DstAddr(), packet.Metadata().Length)
}

func (p *Server) distPort() uint16 {
	return 49152 + p.port%16384
}