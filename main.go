// Command log-analytics is the official NGINX UI log analytics plugin. It
// indexes the nginx access logs, answers structured searches and statistics over
// them, and serves the GeoIP database they need. It speaks the nginx-ui plugin
// protocol on stdin and stdout and serves its HTTP API through the SDK.
package main

import (
	"os"

	sdk "github.com/nginxui/plugin-sdk-go"

	"github.com/nginxui/plugin-log-analytics/internal/app"
	"github.com/nginxui/plugin-log-analytics/internal/memory"
)

func main() {
	memory.Apply()

	plugin := app.New(os.Getenv(sdk.EnvPluginDataDir))
	go plugin.WaitForHost(func() app.Host {
		if host := sdk.CurrentHost(); host != nil {
			return host
		}
		return nil
	})

	sdk.Serve(plugin.Plugin())
}
