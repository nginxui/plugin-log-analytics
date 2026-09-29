package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	sdk "github.com/nginxui/plugin-sdk-go"
	"github.com/nginxui/plugin-sdk-go/jsonrpc"
	"github.com/nginxui/plugin-sdk-go/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "secret-of-this-test"

// TestSDKServesTheAPIOnTheHostSocket runs the plugin through the SDK the way
// the host starts it: the handshake reports the http capability, the API
// answers on the socket in the data directory, and shutdown removes the socket.
func TestSDKServesTheAPIOnTheHostSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports a loopback port instead of a socket")
	}

	dir := shortDir(t)
	t.Setenv(sdk.EnvPluginDataDir, dir)
	t.Setenv(sdk.EnvPluginHTTPSecret, testSecret)
	plugin := New(dir)

	pluginIn, hostW := io.Pipe()
	hostR, pluginOut := io.Pipe()
	host := jsonrpc.NewConn(hostR, hostW)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	go func() { _ = host.Serve(ctx) }()

	runDone := make(chan error, 1)
	go func() { runDone <- sdk.Run(ctx, plugin.Plugin(), pluginIn, pluginOut, sdk.WithoutGRPC()) }()
	t.Cleanup(func() {
		cancel()
		host.Close()
		_ = hostW.Close()
		_ = hostR.Close()
		<-runDone
	})

	var result protocol.InitializeResult
	require.NoError(t, host.Call(ctx, protocol.MethodInitialize, protocol.InitializeParams{}, &result))
	assert.Equal(t, []string{protocol.CapabilityHTTP}, result.Capabilities)
	assert.Zero(t, result.HTTPPort)

	socket := filepath.Join(dir, sdk.HTTPSocketName)
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	// Without the secret of the host the SDK refuses the request.
	resp, err := client.Get("http://plugin/logs/status")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req, err := http.NewRequest(http.MethodGet, "http://plugin/logs/status", nil)
	require.NoError(t, err)
	req.Header.Set(sdk.HeaderPluginSecret, testSecret)
	resp, err = client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var empty protocol.EmptyResult
	require.NoError(t, host.Call(ctx, protocol.MethodShutdown, nil, &empty))
	assert.NoFileExists(t, socket)
}
