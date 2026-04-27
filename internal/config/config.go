package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	DB DBConfig
	PORT string 
}

type DBConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
}

func (d DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host,d.Port,d.User,d.Password,d.Name,d.SSLMode,
	)
}

func Load()(*Config,error) {
	port,err := strconv.Atoi(getEnv("DB_PORT",""))
	if err != nil {
		return nil,fmt.Errorf("invalid DB_PORT: %w",err)
	}

	return &Config{
		DB: DBConfig{
			Host: getEnv("DB_HOST","localhost"),
			Port: port,
			User: getEnv("DB_USER","postgres"),
			Password: getEnv("DB_PASSWORD",""),
			Name: getEnv("DB_NAME","wallet_service"),
			SSLMode: getEnv("DB_SSLMODE","disable"),
		},
		PORT: getEnv("HTTP_PORT",""),
	},nil 
}

func getEnv(key,fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v 
	}
	return fallback
}