package main

import "gopkg.in/yaml.v3"

type config struct {
	IPlimit                 string `yaml:"ip_limit"`
	DBPath                  string `yaml:"db_path"`
	Port                    uint16 `yaml:"port"`
	ShutdownTimeout         uint16 `yaml:"shutdown_timeout"` // seconds
	CleanInterval           uint16 `yaml:"clean_interval"`   // minutes
	Seed                    uint32 `yaml:"seed"`
	CleanBatchSize          uint32 `yaml:"clean_batch_size"`
	CacheInitializationSize uint64 `yaml:"cache_initialization_size"`
}

var Config config

const DefaultDBPath = "./db"

func loadConfig(data []byte) error {
	if err := yaml.Unmarshal(data, &Config); err != nil {
		return err
	}
	if Config.DBPath == "" {
		Config.DBPath = DefaultDBPath
	}
	if Config.Port == 0 {
		Config.Port = 80
	}
	if Config.ShutdownTimeout == 0 {
		Config.ShutdownTimeout = 5
	}
	return nil
}
