package db

import "time"

type Option func(*poolOptions)

type poolOptions struct {
	maxIdleConns int
	connMaxLife  time.Duration
}

func defaultOptions() poolOptions {
	return poolOptions{
		maxIdleConns: 5,
		connMaxLife:  30 * time.Minute,
	}
}

func WithMaxIdleConns(n int) Option {
	return func(o *poolOptions) {
		o.maxIdleConns = n
	}
}

func WithConnMaxLifetime(d time.Duration) Option {
	return func(o *poolOptions) {
		o.connMaxLife = d
	}
}
