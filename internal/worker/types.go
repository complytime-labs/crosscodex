package worker

import "github.com/complytime-labs/crosscodex/pkg/config"

const defaultQueueGroup = "llm-workers"

// WorkerConfig is an alias for config.WorkerConfig, provided for ergonomic
// access within the worker package. Service binaries obtain a WorkerConfig
// from config.DaemonConfig.Worker (populated by config.Config.ServiceConfig()).
type WorkerConfig = config.WorkerConfig

// queueGroup returns the effective queue group name.
func queueGroup(cfg *WorkerConfig) string {
	if cfg.QueueGroup == "" {
		return defaultQueueGroup
	}
	return cfg.QueueGroup
}
