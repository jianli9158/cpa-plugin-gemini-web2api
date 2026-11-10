package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/Mag1cFall/Gemini-Web2API/internal/accounts"
	"github.com/Mag1cFall/Gemini-Web2API/internal/adapter"
	"github.com/Mag1cFall/Gemini-Web2API/internal/auth"
	"github.com/Mag1cFall/Gemini-Web2API/internal/balancer"
	"github.com/Mag1cFall/Gemini-Web2API/internal/config"
	"github.com/Mag1cFall/Gemini-Web2API/internal/logging"
	"github.com/Mag1cFall/Gemini-Web2API/internal/requestdb"
)

const dataDir = "data"

// Version returns the plugin runtime version.
func Version() string {
	return "1.0.2"
}

func authDir(paths []string) string {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			return path
		}
	}
	return "auth"
}

// WithPinnedAccountID attaches a specific Gemini account ID to the request context.
func WithPinnedAccountID(ctx context.Context, accountID string) context.Context {
	return balancer.WithPinnedAccountID(ctx, accountID)
}

// ContextWithPinnedAccountID is an alias for WithPinnedAccountID.
func ContextWithPinnedAccountID(ctx context.Context, accountID string) context.Context {
	return balancer.WithPinnedAccountID(ctx, accountID)
}

// PinnedAccountIDFromContext returns the pinned account ID from context when set.
func PinnedAccountIDFromContext(ctx context.Context) string {
	return balancer.PinnedAccountIDFromContext(ctx)
}

// PluginRuntime wraps the Gemini-Web2API runtime components for in-process CLIProxyAPI plugin hosting.
type PluginRuntime struct {
	ctx        context.Context
	cancel     context.CancelFunc
	DataDir    string
	AuthDir    string
	Cfg        config.Config
	Pool       *balancer.AccountPool
	Accounts   *accounts.Manager
	Store      *requestdb.Store
	Handler    http.Handler
	StartedAt  time.Time
	stopping   chan struct{}
	stopOnce   sync.Once
	running    atomic.Bool
	startMu    sync.Mutex
	started    bool
}

// NewPluginRuntime initializes an in-process Gemini-Web2API runtime without opening a network listener.
func NewPluginRuntime(ctx context.Context, cfg config.Config, runtimeDataDir string, autoStart bool, guestEnabled bool) (*PluginRuntime, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runtimeCtx, cancel := context.WithCancel(ctx)

	if strings.TrimSpace(runtimeDataDir) == "" {
		runtimeDataDir = dataDir
	}
	if err := os.MkdirAll(runtimeDataDir, 0o700); err != nil {
		cancel()
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	_ = logging.Setup(logging.Options{
		Level:  cfg.LogLevel,
		Format: cfg.LogFormat,
		Writer: io.Discard,
	})
	if cfg.LogFile {
		logPath := filepath.Join(runtimeDataDir, "gemini-web2api.log")
		_ = logging.SetFile(logPath, cfg.LogFile, cfg.LogMaxTotalMB)
	}
	go logging.Watch(runtimeCtx)

	store, err := requestdb.Open(filepath.Join(runtimeDataDir, "requests.db"))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open request db: %w", err)
	}
	store.SetBodyCapture(cfg.RequestBodyLog)

	authDirectory := authDir(cfg.AuthStatePaths)
	if len(cfg.AuthStatePaths) > 0 && strings.TrimSpace(cfg.AuthStatePaths[0]) != "" {
		first := strings.TrimSpace(cfg.AuthStatePaths[0])
		if !strings.HasSuffix(strings.ToLower(first), ".json") {
			authDirectory = first
		}
	}
	if err := os.MkdirAll(authDirectory, 0o700); err != nil {
		_ = store.Close()
		cancel()
		return nil, fmt.Errorf("create auth dir: %w", err)
	}

	config.SetModelMapping(cfg.ModelMapping)
	adapter.ConfigureSessionTTL(cfg.SessionTTL)
	pool := balancer.NewAccountPool(cfg.AccountCooldown, cfg.SessionTTL)
	if stored, err := store.Cooldowns(runtimeCtx, time.Now()); err == nil && len(stored) > 0 {
		restored := make([]balancer.Cooldown, 0, len(stored))
		for _, sc := range stored {
			restored = append(restored, balancer.Cooldown{
				Account: sc.Account,
				Model:   sc.Model,
				Until:   sc.Until,
				Reason:  sc.Reason,
			})
		}
		pool.RestoreCooldowns(restored)
	}
	pool.OnCooldownChange(func(cooldowns []balancer.Cooldown) {
		stored := make([]requestdb.StoredCooldown, 0, len(cooldowns))
		for _, c := range cooldowns {
			stored = append(stored, requestdb.StoredCooldown{
				Account: c.Account,
				Model:   c.Model,
				Until:   c.Until,
				Reason:  c.Reason,
			})
		}
		if err := store.SaveCooldowns(runtimeCtx, stored); err != nil {
			logging.Warn(logging.SourceService, "保存账号冷却失败", "错误", err)
		}
	})
	pool.SetStrategy(cfg.RoutingStrategy)
	seconds, _ := strconv.Atoi(cfg.FirstEventHedge)
	adapter.SetFirstEventHedge(adapter.FirstEventHedge{
		Auto:   cfg.FirstEventHedge == "auto",
		After:  time.Duration(seconds) * time.Second,
		Models: cfg.FirstEventHedgeModels,
	})
	adapter.SetResponseModes(cfg.NonstreamKeepalive, cfg.FakeStream)
	manager := accounts.New(runtimeCtx, pool, accounts.Settings{
		InitTimeout:   cfg.InitTimeout,
		RetryInterval: cfg.AccountCooldown,
		Proxy:         cfg.ProxyURL,
		SaveHistory:   cfg.SaveHistory,
		AuthDir:       authDirectory,
		Concurrency:   cfg.PerAccountConcurrency,
	})

	stopping := make(chan struct{})
	startedAt := time.Now()
	router := NewRouter(Deps{
		Pool:     pool,
		Store:    store,
		Accounts: manager,
		APIKey:   func() string { return "" },
	})

	rt := &PluginRuntime{
		ctx:       runtimeCtx,
		cancel:    cancel,
		DataDir:   runtimeDataDir,
		AuthDir:   authDirectory,
		Cfg:       cfg,
		Pool:      pool,
		Accounts:  manager,
		Store:     store,
		Handler:   router,
		StartedAt: startedAt,
		stopping:  stopping,
	}

	if autoStart {
		go rt.StartBackground(guestEnabled)
	}

	return rt, nil
}

// StartBackground loads accounts from AuthStatePaths and starts the account manager in the background.
func (rt *PluginRuntime) StartBackground(guestEnabled bool) {
	if rt == nil {
		return
	}
	rt.startMu.Lock()
	if rt.started {
		rt.startMu.Unlock()
		return
	}
	rt.started = true
	rt.running.Store(true)
	rt.startMu.Unlock()

	loaded, err := auth.LoadFiles(rt.Cfg.AuthStatePaths)
	if err != nil {
		logging.Warn(logging.SourceService, "加载认证目录失败", "错误", err)
	}
	ids := make([]string, 0, len(loaded))
	for _, acc := range loaded {
		ids = append(ids, acc.ID)
	}
	logging.SetSourceWidth(ids)

	rt.Accounts.Start(loaded)
	if !guestEnabled {
		rt.Accounts.StopGuest()
	}
}

// Close stops all account workers, closes SSE streams, and closes the SQLite request store.
func (rt *PluginRuntime) Close() error {
	if rt == nil {
		return nil
	}
	rt.running.Store(false)
	rt.stopOnce.Do(func() {
		close(rt.stopping)
	})
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.Store != nil {
		return rt.Store.Close()
	}
	return nil
}

// ReadAccountStorageState reads the raw storage-state.json bytes for an account ID.
func (rt *PluginRuntime) ReadAccountStorageState(accountID string) ([]byte, *auth.StorageState, error) {
	if rt == nil {
		return nil, nil, fmt.Errorf("runtime not initialized")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, nil, fmt.Errorf("account_id is empty")
	}
	path := filepath.Join(rt.AuthDir, accountID, "storage-state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var state auth.StorageState
	if err := json.Unmarshal(raw, &state); err != nil {
		return raw, nil, err
	}
	return raw, &state, nil
}
