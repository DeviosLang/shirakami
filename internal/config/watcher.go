package config

import (
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"

	"github.com/DeviosLang/shirakami/internal/logger"
)

// safeFields lists configuration keys that can be hot-reloaded without
// restarting the server. Fields that require restart (db.dsn, redis.addr,
// webhook.secret, etc.) are intentionally excluded.
var safeFields = []string{
	"llm.endpoint",
	"llm.model",
	"llm.max_tokens",
	"llm.request_timeout",
	"server.max_concurrent_analyses",
	"server.default_modes",
}

// Watcher 监听配置文件变更并安全地热更新指定字段。
//
// 只更新"安全字段"（不影响正在运行的分析任务）：
//   - llm.endpoint, llm.model, llm.max_tokens, llm.request_timeout
//   - server.max_concurrent_analyses
//   - server.default_modes
//
// 不热更新的字段（需重启）：db.dsn, redis.addr, webhook.secret 等
type Watcher struct {
	mu       sync.Mutex
	stopped  bool
	onReload func(updated *Config)
}

// NewWatcher 启动配置文件监听，变更时调用 onReload 回调。
// 它直接使用 viper 全局实例（Load 已完成初始化），无需重复设置。
// 返回 *Watcher 供调用方通过 Stop() 停止监听。
//
// onReload 在 viper 内部的 goroutine 中被调用；实现必须是并发安全的。
// 调用方应只读取并应用"安全字段"，忽略 db.dsn / redis.addr 等需要重启的字段。
func NewWatcher(onReload func(updated *Config)) (*Watcher, error) {
	w := &Watcher{onReload: onReload}

	viper.OnConfigChange(func(e fsnotify.Event) {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		w.mu.Unlock()

		log := logger.S()

		var updated Config
		if err := viper.Unmarshal(&updated); err != nil {
			log.Warnw("config.reload_failed", "err", err, "file", e.Name)
			return
		}

		// Log which safe fields are present in the reloaded config.
		changed := make([]string, 0, len(safeFields))
		for _, key := range safeFields {
			if viper.IsSet(key) {
				changed = append(changed, key)
			}
		}

		log.Infow("config.reloaded",
			"file", e.Name,
			"op", e.Op.String(),
			"llm.endpoint", updated.LLM.Endpoint,
			"llm.model", updated.LLM.Model,
			"llm.max_tokens", updated.LLM.MaxTokens,
			"llm.request_timeout", updated.LLM.RequestTimeout,
			"server.max_concurrent_analyses", updated.Server.MaxConcurrentAnalyses,
			"server.default_modes", updated.Server.DefaultModes,
			"safe_fields", changed,
		)

		w.onReload(&updated)
	})

	viper.WatchConfig()

	return w, nil
}

// Stop 停止配置文件监听。
// 后续的文件变更事件将被忽略；已在执行中的 onReload 回调不会被中断。
// Stop 不调用 viper.Reset()，以免影响其他使用 viper 全局实例的代码。
func (w *Watcher) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
}
