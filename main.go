// Command log-analytics is the official NGINX UI log analytics plugin. It
// indexes the nginx access logs, answers structured searches and statistics over
// them, and serves the GeoIP database they need. It speaks the nginx-ui plugin
// protocol on stdin and stdout and serves its HTTP API on a private socket.
package main

import (
	"os"

	sdk "github.com/nginxui/plugin-sdk-go"

	"github.com/nginxui/plugin-log-analytics/internal/app"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/memory"
)

func main() {
	memory.Apply()

	plugin := app.New(os.Getenv(sdk.EnvPluginDataDir))
	if err := plugin.ListenHTTP(); err != nil {
		// The plugin still answers the protocol, so the host can show it as
		// broken instead of failing the handshake.
		logger.Errorf("The HTTP API is not available: %v", err)
	}

	go plugin.WaitForHost(func() app.Host {
		if host := sdk.CurrentHost(); host != nil {
			return host
		}
		return nil
	})

	sdk.Serve(plugin.Plugin())
}
