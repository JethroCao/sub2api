package config

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestCredentialBrokerConfigValidation(t *testing.T) {
	require.NoError(t, (CredentialBrokerConfig{}).Validate())
	valid := CredentialBrokerConfig{Enabled: true, Clients: []CredentialBrokerClientConfig{{ID: "node-107", KeySHA256: strings.Repeat("a", 64), AccountIDs: []int64{4, 14}}}, TrustedProxyCIDRs: []string{"127.0.0.1/32"}}
	require.NoError(t, valid.Validate())
	cases := map[string]func(*CredentialBrokerConfig){
		"no clients":        func(c *CredentialBrokerConfig) { c.Clients = nil },
		"bad digest":        func(c *CredentialBrokerConfig) { c.Clients[0].KeySHA256 = "secret" },
		"empty grant":       func(c *CredentialBrokerConfig) { c.Clients[0].AccountIDs = nil },
		"wildcard account":  func(c *CredentialBrokerConfig) { c.Clients[0].AccountIDs = []int64{0} },
		"duplicate account": func(c *CredentialBrokerConfig) { c.Clients[0].AccountIDs = []int64{4, 4} },
		"unsafe client id":  func(c *CredentialBrokerConfig) { c.Clients[0].ID = "../node" },
		"duplicate client":  func(c *CredentialBrokerConfig) { c.Clients = append(c.Clients, c.Clients[0]) },
		"duplicate key":     func(c *CredentialBrokerConfig) { x := c.Clients[0]; x.ID = "other"; c.Clients = append(c.Clients, x) },
		"trust everyone":    func(c *CredentialBrokerConfig) { c.TrustedProxyCIDRs = []string{"0.0.0.0/0"} },
		"invalid proxy":     func(c *CredentialBrokerConfig) { c.TrustedProxyCIDRs = []string{"not-a-cidr"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid
			c.Clients = append([]CredentialBrokerClientConfig(nil), valid.Clients...)
			mutate(&c)
			require.Error(t, c.Validate())
		})
	}
}

func TestCredentialBrokerConfigLoadAndProxyEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("CREDENTIAL_BROKER_ENABLED", "true")
	t.Setenv("CREDENTIAL_BROKER_TRUSTED_PROXY_CIDRS", "127.0.0.1/32")
	viper.Set("credential_broker.clients", []map[string]any{{"id": "node-107", "key_sha256": strings.Repeat("a", 64), "account_ids": []int64{4, 14}}})
	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.CredentialBroker.Enabled)
	require.Equal(t, []string{"127.0.0.1/32"}, cfg.CredentialBroker.TrustedProxyCIDRs)
	require.Equal(t, []int64{4, 14}, cfg.CredentialBroker.Clients[0].AccountIDs)
}
