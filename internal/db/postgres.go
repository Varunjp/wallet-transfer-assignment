package db

import (
	"context"
	"fmt"
	"time"
	"wallet-service/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxConns        	= 20
	minConns        	= 2
	maxConnLifetime 	= 30 * time.Minute
	maxConnIdleTime 	= 5 * time.Minute
	healthCheckPeriod 	= 1 * time.Minute
	connectTimeout 		= 5 * time.Second
)

func NewPool (ctx context.Context, cfg config.DBConfig)(*pgxpool.Pool,error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse db config: %w",err)
	}

	poolCfg.MaxConns = maxConns
	poolCfg.MinConns = minConns
	poolCfg.MaxConnLifetime = maxConnLifetime
	poolCfg.MaxConnIdleTime = maxConnIdleTime
	poolCfg.HealthCheckPeriod = healthCheckPeriod
	poolCfg.ConnConfig.ConnectTimeout = connectTimeout

	pool,err := pgxpool.NewWithConfig(ctx,poolCfg)
	if err != nil {
		return nil,fmt.Errorf("create pool: %w",err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil,fmt.Errorf("ping db: %w",err)
	}

	return pool,nil 
}