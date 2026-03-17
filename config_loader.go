package qqbot

import "github.com/sky22333/qqbot/config"

func LoadConfig(path string) (Config, error) {
	return config.Load(path)
}

func DefaultConfig() Config {
	return config.Default()
}
