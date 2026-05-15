package db

import "time"

type IsolationLevel string

const (
	IsolationReadUncommitted IsolationLevel = "READ UNCOMMITTED"
	IsolationReadCommitted   IsolationLevel = "READ COMMITTED"
	IsolationRepeatableRead  IsolationLevel = "REPEATABLE READ"
	IsolationSerializable    IsolationLevel = "SERIALIZABLE"
)

type Row interface {
	Scan(dest ...any) error
}

type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Close() error
	Err() error
}

type HealthStatus struct {
	Connected    bool
	OpenConns    int
	InUse        int
	Idle         int
	MaxOpen      int
	WaitCount    int64
	WaitDuration time.Duration
}

type PoolConfig struct {
	DSN          string
	MaxOpenConns int
	MaxIdleConns int
	ConnMaxLife  time.Duration
	SSLMode      string
	Extensions   []string
}
