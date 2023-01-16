package nmproxy

import (
	"context"
	"fmt"
	"sync"

	"github.com/gravitl/netclient/nmproxy/config"
	"github.com/gravitl/netclient/nmproxy/manager"
	"github.com/gravitl/netclient/nmproxy/server"
	"github.com/gravitl/netclient/nmproxy/stun"
	"github.com/gravitl/netmaker/logger"
	nm_models "github.com/gravitl/netmaker/models"
)

// Start - setups the global cfg for proxy and starts the proxy server
func Start(ctx context.Context, wg *sync.WaitGroup, mgmChan chan *nm_models.HostPeerUpdate, stunAddr string, stunPort int) {

	if config.GetCfg().IsProxyRunning() {
		logger.Log(1, "Proxy is running already...")
		return
	}
	logger.Log(1, "Starting Proxy...")
	config.GetCfg().SetProxyStatus(true)
	defer wg.Done()
	if stunAddr == "" || stunPort == 0 {
		logger.Log(1, "stun config values cannot be empty")
		return
	}
	config.InitializeCfg()
	defer config.Reset()
	config.GetCfg().SetHostInfo(stun.GetHostInfo(stunAddr, stunPort))
	logger.Log(0, fmt.Sprintf("HOSTINFO: %+v", config.GetCfg().GetHostInfo()))
	config.GetCfg().SetNATStatus()
	// start the netclient proxy server
	err := server.NmProxyServer.CreateProxyServer(config.GetCfg().GetHostInfo().PrivPort, 0, config.GetCfg().GetHostInfo().PrivIp.String())
	if err != nil {
		logger.FatalLog("failed to create proxy: ", err.Error())
	}
	config.GetCfg().SetServerConn(server.NmProxyServer.Server)
	go manager.Start(ctx, mgmChan)
	server.NmProxyServer.Listen(ctx)
}

func IsProxyRunning() bool {
	return config.GetCfg().IsProxyRunning()
}
