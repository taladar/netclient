package manager

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/gravitl/netclient/ncutils"
	"github.com/gravitl/netclient/nmproxy/config"
	"github.com/gravitl/netclient/nmproxy/packet"
	"github.com/gravitl/netclient/nmproxy/peer"

	"github.com/gravitl/netclient/nmproxy/models"
	peerpkg "github.com/gravitl/netclient/nmproxy/peer"
	"github.com/gravitl/netclient/nmproxy/wg"
	"github.com/gravitl/netmaker/logger"
	nm_models "github.com/gravitl/netmaker/models"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type proxyPayload models.ProxyManagerPayload

func getRecieverType(m *models.ProxyManagerPayload) *proxyPayload {
	mI := proxyPayload(*m)
	return &mI
}

// Start - starts the proxy manager loop and listens for events on the Channel provided
func Start(ctx context.Context, managerChan chan *nm_models.HostPeerUpdate) {
	for {
		select {
		case <-ctx.Done():
			logger.Log(1, "shutting down proxy manager...")
			return
		case mI := <-managerChan:
			if mI == nil {
				continue
			}
			logger.Log(0, fmt.Sprintf("-------> PROXY-MANAGER: %+v\n", mI.ProxyUpdate))
			err := configureProxy(mI)
			if err != nil {
				logger.Log(0, "failed to configure proxy:  ", err.Error())
			}
		}
	}
}

// configureProxy - confgures proxy by payload action
func configureProxy(payload *nm_models.HostPeerUpdate) error {
	var err error
	m := getRecieverType(&payload.ProxyUpdate)
	m.InterfaceName = ncutils.GetInterfaceName()
	m.Peers = payload.Peers
	noProxy(payload) // starts or stops the metrics collection based on host proxy setting
	if m.Action == models.ProxyUpdate {
		m.peerUpdate()
	}
	return err
}

func noProxy(peerUpdate *nm_models.HostPeerUpdate) {
	config.GetCfg().SetPeers(peerUpdate.PeerIDs)
	if peerUpdate.ProxyUpdate.Action != models.NoProxy && config.GetCfg().GetMetricsCollectionStatus() {
		// stop the metrics thread since proxy is switched on for the host
		logger.Log(0, "Stopping Metrics Thread...")
		config.GetCfg().StopMetricsCollectionThread()
	} else if peerUpdate.ProxyUpdate.Action == models.NoProxy && !config.GetCfg().GetMetricsCollectionStatus() {
		ctx, cancel := context.WithCancel(context.Background())
		go peer.StartMetricsCollectionForHostPeers(ctx)
		config.GetCfg().SetMetricsThreadCtx(cancel)
	}
}

// ProxyManagerPayload.settingsUpdate - updates the network settings in the config
func (m *proxyPayload) settingsUpdate() (reset bool) {
	if !m.IsRelay && config.GetCfg().IsRelay() {
		config.GetCfg().DeleteRelayedPeers()
	}
	if m.IsIngress {
		packet.TurnOffIpFowarding()
	}
	if m.IsIngress && !config.GetCfg().CheckIfRouterIsRunning() {
		// start router on the ingress node
		config.GetCfg().SetRouterToRunning()
		go packet.StartRouter()

	} else if !m.IsIngress && config.GetCfg().CheckIfRouterIsRunning() {
		config.GetCfg().StopRouter()
	}
	config.GetCfg().SetRelayStatus(m.IsRelay)
	config.GetCfg().SetIngressGwStatus(m.IsIngress)
	if config.GetCfg().GetRelayedStatus() != m.IsRelayed {
		reset = true
	}
	config.GetCfg().SetRelayedStatus(m.IsRelayed)
	if m.IsRelay {
		m.setRelayedPeers()
	}
	return
}

// ProxyManagerPayload.setRelayedPeers - processes the payload for the relayed peers
func (m *proxyPayload) setRelayedPeers() {
	c := config.GetCfg()
	for relayedNodePubKey, relayedNodeConf := range m.RelayedPeerConf {
		for _, peer := range relayedNodeConf.Peers {
			if peer.Endpoint != nil {
				peer.Endpoint.Port = models.NmProxyPort
				rPeer := models.RemotePeer{
					PeerKey:  peer.PublicKey.String(),
					Endpoint: peer.Endpoint,
				}
				c.SaveRelayedPeer(relayedNodePubKey, &rPeer)

			}

		}
		relayedNodeConf.RelayedPeerEndpoint.Port = models.NmProxyPort
		relayedNode := models.RemotePeer{
			PeerKey:  relayedNodePubKey,
			Endpoint: relayedNodeConf.RelayedPeerEndpoint,
		}
		c.SaveRelayedPeer(relayedNodePubKey, &relayedNode)

	}
}

func cleanUpInterface() {
	logger.Log(1, "cleaning up proxy peer connections")
	peerConnMap := config.GetCfg().GetAllProxyPeers()
	for _, peerI := range peerConnMap {
		config.GetCfg().RemovePeer(peerI.Key.String())
	}
	noProxyPeers := config.GetCfg().GetNoProxyPeers()
	for _, peerI := range noProxyPeers {
		config.GetCfg().DeleteNoProxyPeer(peerI.Config.PeerEndpoint.IP.String())
	}

}

// ProxyManagerPayload.processPayload - updates the peers and config with the recieved payload
func (m *proxyPayload) processPayload() error {
	var err error
	var wgIface *wg.WGIface
	if m.InterfaceName == "" {
		return errors.New("interface cannot be empty")
	}
	if len(m.Peers) == 0 {
		return errors.New("no peers to add")
	}
	reset := m.settingsUpdate()
	if reset {
		cleanUpInterface()
		return nil
	}
	gCfg := config.GetCfg()
	wgIface, err = wg.GetWgIface(m.InterfaceName)
	if err != nil {
		logger.Log(1, "Failed get interface config: ", err.Error())
		return err
	}
	gCfg.SetIface(wgIface)
	// sync map with wg device config
	// check if listen port has changed
	if wgIface.Device.ListenPort != gCfg.GetInterfaceListenPort() {
		// reset proxy
		cleanUpInterface()
		return nil
	}
	peerConnMap := gCfg.GetAllProxyPeers()
	noProxyPeerMap := gCfg.GetNoProxyPeers()
	// check device conf different from proxy
	// sync peer map with new update
	for peerPubKey, peerConn := range peerConnMap {
		if _, ok := m.PeerMap[peerPubKey]; !ok {

			if peerConn.IsAttachedExtClient {
				logger.Log(1, "------> Deleting ExtClient Watch Thread: ", peerConn.Key.String())
				gCfg.DeleteExtWaitCfg(peerConn.Key.String())
				gCfg.DeleteExtClientInfo(peerConn.Config.PeerConf.Endpoint)
			}
			gCfg.RemovePeer(peerConn.Key.String())
		}
	}

	// update no proxy peers map with peer update
	for peerIP, peerConn := range noProxyPeerMap {
		if _, ok := m.PeerMap[peerConn.Key.String()]; !ok {
			gCfg.DeleteNoProxyPeer(peerIP)
		}
	}

	for i := len(m.Peers) - 1; i >= 0; i-- {

		if currentPeer, ok := peerConnMap[m.Peers[i].PublicKey.String()]; ok {
			currentPeer.Mutex.Lock()
			if currentPeer.IsAttachedExtClient {
				_, found := gCfg.GetExtClientInfo(currentPeer.Config.PeerEndpoint)
				if found {
					m.Peers = append(m.Peers[:i], m.Peers[i+1:]...)
					currentPeer.Mutex.Unlock()

				}
				continue

			}
			// check if proxy is off for the peer
			if !m.PeerMap[m.Peers[i].PublicKey.String()].Proxy {

				// cleanup proxy connections for the peer
				currentPeer.StopConn()
				delete(peerConnMap, currentPeer.Key.String())
				//m.Peers = append(m.Peers[:i], m.Peers[i+1:]...)
				currentPeer.Mutex.Unlock()
				continue

			}
			// check if peer is not connected to proxy
			devPeer, err := wg.GetPeer(m.InterfaceName, currentPeer.Key.String())
			if err == nil {
				logger.Log(0, "---------> COMPARING ENDPOINT: DEV: %s, Proxy: %s", devPeer.Endpoint.String(), currentPeer.Config.LocalConnAddr.String())
				if devPeer.Endpoint.String() != currentPeer.Config.LocalConnAddr.String() {
					logger.Log(1, "---------> endpoint is not set to proxy: ", currentPeer.Key.String())
					currentPeer.StopConn()
					currentPeer.Mutex.Unlock()
					delete(peerConnMap, currentPeer.Key.String())
					continue
				}
			}

			//check if peer is being relayed
			if currentPeer.IsRelayed != m.PeerMap[m.Peers[i].PublicKey.String()].IsRelayed {
				logger.Log(1, "---------> peer relay status has been changed: ", currentPeer.Key.String())
				currentPeer.StopConn()
				currentPeer.Mutex.Unlock()
				delete(peerConnMap, currentPeer.Key.String())
				continue
			}

			// check if relay endpoint has been changed
			if currentPeer.RelayedEndpoint != nil &&
				m.PeerMap[m.Peers[i].PublicKey.String()].RelayedTo != nil &&
				currentPeer.RelayedEndpoint.String() != m.PeerMap[m.Peers[i].PublicKey.String()].RelayedTo.String() {
				logger.Log(1, "---------> peer relay endpoint has been changed: ", currentPeer.Key.String())
				currentPeer.StopConn()
				currentPeer.Mutex.Unlock()
				delete(peerConnMap, currentPeer.Key.String())
				continue
			}

			// check if proxy listen port has changed for the peer
			if currentPeer.Config.ListenPort != int(m.PeerMap[m.Peers[i].PublicKey.String()].PublicListenPort) &&
				m.PeerMap[m.Peers[i].PublicKey.String()].PublicListenPort != 0 {
				// listen port has been changed, reset conn
				logger.Log(1, "--------> peer proxy listen port has been changed", currentPeer.Key.String())
				currentPeer.StopConn()
				currentPeer.Mutex.Unlock()
				delete(peerConnMap, currentPeer.Key.String())
				continue
			}

			if currentPeer.Config.RemoteConnAddr.IP.String() != m.Peers[i].Endpoint.IP.String() {
				logger.Log(1, "----------> Resetting proxy for Peer: ", currentPeer.Key.String())
				currentPeer.StopConn()
				currentPeer.Mutex.Unlock()
				delete(peerConnMap, currentPeer.Key.String())
				continue

			}
			// delete the peer from the list
			logger.Log(1, "-----------> No updates observed so deleting peer: ", m.Peers[i].PublicKey.String())
			// peer exists and no changes observed, update network map for the peer
			// currentPeer.NetworkSettings[m.Network] = models.Settings{
			// 	IsRelayed: m.PeerMap[m.Peers[i].PublicKey.String()].IsRelayed,
			// 	RelayedTo: m.PeerMap[m.Peers[i].PublicKey.String()].RelayedTo,
			// }
			peerConnMap[currentPeer.Key.String()] = currentPeer
			m.Peers = append(m.Peers[:i], m.Peers[i+1:]...)
			currentPeer.Mutex.Unlock()
			continue

		}

		if noProxypeer, found := noProxyPeerMap[m.Peers[i].Endpoint.IP.String()]; found {
			if m.PeerMap[m.Peers[i].PublicKey.String()].Proxy {
				// cleanup proxy connections for the no proxy peer since proxy is switched on for the peer
				noProxypeer.Mutex.Lock()
				noProxypeer.StopConn()
				noProxypeer.Mutex.Unlock()
				delete(noProxyPeerMap, noProxypeer.Config.PeerEndpoint.IP.String())
				continue
			}
			// update network map
			// noProxypeer.NetworkSettings[m.Network] = models.Settings{
			// 	IsRelayed: m.PeerMap[m.Peers[i].PublicKey.String()].IsRelayed,
			// 	RelayedTo: m.PeerMap[m.Peers[i].PublicKey.String()].RelayedTo,
			// }
			noProxyPeerMap[noProxypeer.Key.String()] = noProxypeer
			m.Peers = append(m.Peers[:i], m.Peers[i+1:]...)
		}

	}

	gCfg.UpdateProxyPeers(&peerConnMap)
	gCfg.UpdateNoProxyPeers(&noProxyPeerMap)
	logger.Log(1, "CLEANED UP..........")
	return nil
}

// ProxyManagerPayload.peerUpdate - porcesses the peer update
func (m *proxyPayload) peerUpdate() error {

	err := m.processPayload()
	if err != nil {
		return err
	}
	for _, peerI := range m.Peers {

		peerConf := m.PeerMap[peerI.PublicKey.String()]
		if peerI.Endpoint == nil && !peerConf.IsAttachedExtClient {
			logger.Log(1, "Endpoint nil for peer: ", peerI.PublicKey.String())
			continue
		}

		var isRelayed bool
		var relayedTo *net.UDPAddr
		if m.IsRelayed {
			isRelayed = true
			relayedTo = m.RelayedTo
		} else {

			isRelayed = peerConf.IsRelayed
			relayedTo = peerConf.RelayedTo

		}
		if peerConf.IsAttachedExtClient {
			if _, found := config.GetCfg().GetExtClientWaitCfg(peerI.PublicKey.String()); found {
				continue
			}
			logger.Log(1, "extclient watch thread starting for: ", peerI.PublicKey.String())
			go func(peer *wgtypes.PeerConfig, isRelayed bool, relayTo *net.UDPAddr,
				peerConf models.PeerConf, ingGwAddr string) {
				addExtClient := false
				commChan := make(chan *net.UDPAddr, 30)
				ctx, cancel := context.WithCancel(context.Background())
				extPeer := models.RemotePeer{
					PeerKey:             peer.PublicKey.String(),
					CancelFunc:          cancel,
					CommChan:            commChan,
					IsAttachedExtClient: true,
				}
				config.GetCfg().SaveExtclientWaitCfg(&extPeer)
				defer func() {
					if addExtClient {
						logger.Log(1, "GOT ENDPOINT for Extclient adding peer...", extPeer.Endpoint.String())
						peerpkg.AddNew(&peerI, peerConf, isRelayed, relayedTo)
					}
					logger.Log(1, "Exiting extclient watch Thread for: ", peer.PublicKey.String())
				}()
				for {
					select {
					case <-ctx.Done():
						return
					case endpoint := <-commChan:
						if endpoint != nil {
							addExtClient = true
							peer.Endpoint = endpoint
							config.GetCfg().DeleteExtWaitCfg(peer.PublicKey.String())
							return
						}
					}

				}

			}(&peerI, isRelayed, relayedTo, peerConf, m.WgAddr)
			continue
		}

		peerpkg.AddNew(&peerI, peerConf, isRelayed, relayedTo)

	}
	return nil
}
