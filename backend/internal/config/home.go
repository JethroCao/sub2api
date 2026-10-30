package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type HomeConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	Address    string `mapstructure:"address"`
	ServerName string `mapstructure:"server_name"`
	CAFile     string `mapstructure:"ca_file"`
	CertFile   string `mapstructure:"cert_file"`
	KeyFile    string `mapstructure:"key_file"`
	APIKey     string `mapstructure:"api_key"`
}

func (c HomeConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	host, port, err := net.SplitHostPort(c.Address)
	n, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || n < 1 || n > 65535 || strings.Contains(c.Address, "://") {
		return fmt.Errorf("home.address must be host:port")
	}
	for name, value := range map[string]string{"server_name": c.ServerName, "ca_file": c.CAFile, "cert_file": c.CertFile, "key_file": c.KeyFile, "api_key": c.APIKey} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("home.%s is required when enabled", name)
		}
	}
	return nil
}
