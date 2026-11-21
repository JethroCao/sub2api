package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// CredentialBrokerConfig grants trusted nodes access to ATs, never refresh tokens.
// It is intentionally separate from user API keys and disabled by default.
type CredentialBrokerConfig struct {
	Enabled           bool                           `mapstructure:"enabled"`
	Clients           []CredentialBrokerClientConfig `mapstructure:"clients"`
	TrustedProxyCIDRs []string                       `mapstructure:"trusted_proxy_cidrs"`
}
type CredentialBrokerClientConfig struct {
	ID         string  `mapstructure:"id"`
	KeySHA256  string  `mapstructure:"key_sha256"`
	AccountIDs []int64 `mapstructure:"account_ids"`
}

var brokerClientID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (c CredentialBrokerConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if len(c.Clients) == 0 || len(c.Clients) > 100 {
		return fmt.Errorf("credential_broker requires 1..100 clients")
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, client := range c.Clients {
		if !brokerClientID.MatchString(client.ID) || ids[client.ID] {
			return fmt.Errorf("credential_broker client IDs must be safe and unique")
		}
		ids[client.ID] = true
		digest, err := hex.DecodeString(client.KeySHA256)
		if err != nil || len(digest) != 32 || keys[strings.ToLower(client.KeySHA256)] {
			return fmt.Errorf("credential_broker key_sha256 must be a unique SHA-256 hex digest")
		}
		keys[strings.ToLower(client.KeySHA256)] = true
		if len(client.AccountIDs) == 0 || len(client.AccountIDs) > 1000 {
			return fmt.Errorf("credential_broker clients require 1..1000 explicit account IDs")
		}
		seen := map[int64]bool{}
		for _, id := range client.AccountIDs {
			if id <= 0 || seen[id] {
				return fmt.Errorf("credential_broker account IDs must be positive and unique")
			}
			seen[id] = true
		}
	}
	for _, cidr := range c.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("credential_broker trusted_proxy_cidrs must contain CIDRs")
		}
		ones, _ := network.Mask.Size()
		if ones == 0 {
			return fmt.Errorf("credential_broker cannot trust all proxy addresses")
		}
	}
	return nil
}
