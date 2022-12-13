package router

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/gravitl/netclient/nmproxy/config"
	"github.com/gravitl/netmaker/logger"
)

/*
	For egress -
	1. fetch all addresses from the wg interface
	2. two sniffers - 1) for WG interface --> with filter dst set to gateway interfaces --> then inject the pkt to GW interface
					  2) for GW interface --> with filter dst set to wg interface addrs --> then inject the pkt to Wg interface
*/

var (
	snapshotLen int32         = 65048
	promiscuous bool          = true
	timeout     time.Duration = 1 * time.Microsecond
)

// StartIngress - sniffs the the interface
func StartIngress() error {
	var err error
	defer func() {
		config.GetCfg().ResetIngressRouter()
		if err != nil {
			logger.Log(0, "---------> Failed to start router: ", err.Error())
		}
		logger.Log(0, "-----> Stopping Router...")
	}()
	if config.GetCfg().IsIfaceNil() {
		return errors.New("iface is nil")
	}
	ifaceName := config.GetCfg().GetIface().Name
	logger.Log(1, "Starting Packet router for iface: ", ifaceName)
	outHandler, err := getIngressOutboundHandler(ifaceName)
	if err != nil {
		return err
	}
	inHandler, err := getIngressInboundHandler(ifaceName)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	config.GetCfg().SetIngressRouterHandlers(inHandler, outHandler, cancel)
	err = config.GetCfg().SetIngressBPFFilter()
	if err != nil {
		return err
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go startIngressInBoundRouter(ctx, wg)
	wg.Add(1)
	go startIngressOutBoundRouter(ctx, wg)
	wg.Wait()
	return nil
}

func printPktInfo(packet gopacket.Packet, inbound bool) {

	ipLayer := packet.Layer(layers.LayerTypeIPv4)
	if ipLayer != nil {
		fmt.Println("IPv4 layer detected.  ", inbound)
		ip, _ := ipLayer.(*layers.IPv4)
		fmt.Printf("From %s to %s\n", ip.SrcIP, ip.DstIP)
		fmt.Println("Protocol: ", ip.Protocol)
		fmt.Println()

	}
	// Check for errors
	if err := packet.ErrorLayer(); err != nil {
		fmt.Println("Error decoding some part of the packet:", err)
	}
}

func routePkt(pkt gopacket.Packet, inbound bool) ([]byte, bool) {
	if pkt.NetworkLayer() != nil {
		flow := pkt.NetworkLayer().NetworkFlow()
		src, dst := flow.Endpoints()
		var srcIP, dstIP net.IP
		if inbound {
			if rInfo, found := config.GetCfg().GetIngressRoutingInfo(src.String(), inbound); found {
				srcIP = rInfo.InternalIP
				dstIP = net.ParseIP(dst.String())
			}
		} else {
			if rInfo, found := config.GetCfg().GetIngressRoutingInfo(dst.String(), inbound); found {
				srcIP = net.ParseIP(src.String())
				dstIP = rInfo.ExternalIP
			}
		}
		if srcIP != nil && dstIP != nil {
			if pkt.NetworkLayer().(*layers.IPv4) != nil {
				pkt.NetworkLayer().(*layers.IPv4).SrcIP = srcIP
				pkt.NetworkLayer().(*layers.IPv4).DstIP = dstIP
			} else if pkt.NetworkLayer().(*layers.IPv6) != nil {
				pkt.NetworkLayer().(*layers.IPv6).SrcIP = srcIP
				pkt.NetworkLayer().(*layers.IPv6).DstIP = dstIP
			}
			buffer := gopacket.NewSerializeBuffer()
			options := gopacket.SerializeOptions{
				ComputeChecksums: true,
				FixLengths:       true,
			}

			if pkt.TransportLayer() != nil && pkt.TransportLayer().(*layers.TCP) != nil {
				pkt.TransportLayer().(*layers.TCP).SetNetworkLayerForChecksum(pkt.NetworkLayer())
			}

			// Serialize Packet to get raw bytes
			if err := gopacket.SerializePacket(buffer, options, pkt); err != nil {
				logger.Log(0, "Failed to serialize packet: ", err.Error())
				return nil, false
			}
			packetBytes := buffer.Bytes()
			return packetBytes, true
		}

	}
	return nil, false
}