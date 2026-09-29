// Package app wires the plugin to the host: it hands the HTTP API to the SDK to
// serve, reads settings and the log list from the host, starts the indexing
// services once the host says so, and stops them on shutdown.
package app

import (
	"context"
	"net/http"
	"sync"
	"time"

	sdk "github.com/nginxui/plugin-sdk-go"
	"github.com/nginxui/plugin-sdk-go/protocol"

	"github.com/nginxui/plugin-log-analytics/internal/api"
	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/demo"
	"github.com/nginxui/plugin-log-analytics/internal/legacy"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

const (
	// ActivityKey and ActivityLabel are the entry of the host processing
	// indicator that shows while logs are indexed. The label is an English
	// source string, the browser bundle translates it.
	ActivityKey   = "indexing"
	ActivityLabel = "Nginx Log Indexing..."

	// shutdownWait bounds how long shutdown waits for a running round.
	shutdownWait = 20 * time.Second
)

// Host is what the app needs from the host client. *sdk.Host implements it.
type Host interface {
	Ready() bool
	Info() sdk.Info
	Settings() map[string]any
	LogsList(ctx context.Context) ([]protocol.HostLogFile, error)
	ActivitySet(ctx context.Context, key, label string, active bool) error
	legacy.KV
}

// App is the plugin process.
type App struct {
	dataDir string
	handler http.Handler

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	scheduler *service.IncrementalScheduler
	host      Host
	started   bool
}

// New returns an app that keeps its files in dataDir.
func New(dataDir string) *App {
	ctx, cancel := context.WithCancel(context.Background())
	return &App{dataDir: dataDir, handler: api.NewRouter(), ctx: ctx, cancel: cancel}
}

// Plugin returns the SDK description of the plugin.
func (a *App) Plugin() sdk.Plugin {
	return sdk.Plugin{
		// The SDK listens for the host and serves the API. Until the services
		// start, the handlers answer that the index is not available.
		HTTP: a.handler,
		Configure: func(_ context.Context, settings map[string]any) error {
			config.Set(config.ParseSettings(settings))
			return nil
		},
		Events: map[string]sdk.EventHandler{
			protocol.EventLogPathsChanged: func(ctx context.Context, _ protocol.EventNotification) {
				a.refreshLogs(ctx)
			},
		},
		Shutdown: a.Shutdown,
	}
}

// WaitForHost blocks until the host is ready for host calls, then starts the
// services. It gives up when the app is shut down first.
func (a *App) WaitForHost(get func() Host) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			if host := get(); host != nil && host.Ready() {
				if err := a.Start(host); err != nil {
					logger.Errorf("Log analytics failed to start: %v", err)
				}
				return
			}
		}
	}
}

// Start brings the services up. The host is ready for host calls.
func (a *App) Start(host Host) error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = true
	a.host = host
	a.mu.Unlock()

	config.SetDataDir(a.dataDir)
	config.Set(config.ParseSettings(host.Settings()))

	// The old data of a host that kept the log analytics itself comes first: the
	// index and its metadata have to be in place before the services read them.
	if _, err := store.Open(a.dataDir); err != nil {
		service.MarkStartFailed(nil)
		return err
	}
	if _, err := legacy.Consume(a.ctx, a.dataDir, host); err != nil {
		// The import directory stays and the next start tries again. The
		// services still start, on whatever is in place.
		logger.Errorf("Could not take over the data of the host: %v", err)
	}
	legacy.ResolveKV(a.ctx, host)

	if applied := demo.Install(); len(applied) > 0 {
		logger.Infof("Demo mode, installed: %v", applied)
	}

	a.refreshLogs(a.ctx)

	service.SetActivityHook(func(indexing bool) { a.setActivity(indexing) })
	service.InitializeServices(a.ctx)

	scheduler := service.StartIncrementalScheduler(a.ctx)
	config.Subscribe(func(prev, next config.Settings) {
		if prev.IncrementalIndexInterval != next.IncrementalIndexInterval {
			scheduler.Reset()
		}
		if prev.MaxConcurrentIndexTasks != next.MaxConcurrentIndexTasks {
			service.NotifySettingsChanged()
		}
	})

	a.mu.Lock()
	a.scheduler = scheduler
	a.mu.Unlock()
	return nil
}

// refreshLogs asks the host which log files the plugin may read and hands the
// list to the services. The host sends log.paths_changed when it changes.
func (a *App) refreshLogs(ctx context.Context) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host == nil {
		return
	}

	logs, err := host.LogsList(ctx)
	if err != nil {
		logger.Errorf("Could not list the nginx log files: %v", err)
		return
	}

	list := make([]utils.HostLog, 0, len(logs))
	for _, log := range logs {
		list = append(list, utils.HostLog{
			Path:       log.Path,
			Type:       log.Type,
			Source:     log.Source,
			ConfigFile: log.ConfigFile,
		})
	}
	service.SetHostLogs(list)
	logger.Debugf("The host lists %d nginx log file(s)", len(list))
}

// setActivity mirrors the indexing state to the host indicator.
func (a *App) setActivity(indexing bool) {
	a.mu.Lock()
	host := a.host
	a.mu.Unlock()
	if host == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.ActivitySet(ctx, ActivityKey, ActivityLabel, indexing); err != nil {
		logger.Debugf("Could not update the host activity: %v", err)
	}
}

// Shutdown stops the scheduler and the services. The SDK has already closed the
// HTTP listener by then and waits for the requests still running afterwards.
func (a *App) Shutdown(_ context.Context) error {
	a.cancel()

	waitCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()

	a.mu.Lock()
	scheduler := a.scheduler
	a.scheduler = nil
	a.mu.Unlock()

	if scheduler != nil {
		scheduler.Stop(waitCtx)
	}

	service.StopServices()
	service.SetActivityHook(nil)
	a.setActivity(false)

	return store.Close()
}
