package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestHomeConfigValidation(t *testing.T) {
	require.NoError(t, (HomeConfig{}).Validate())
	require.Error(t, (HomeConfig{Enabled: true}).Validate())
	c := HomeConfig{Enabled: true, Address: "home:8443", ServerName: "home", CAFile: "ca.pem", CertFile: "cert.pem", KeyFile: "key.pem", APIKey: "configured"}
	require.NoError(t, c.Validate())
	c.Address = "http://home:8443"
	require.Error(t, c.Validate())
}

func TestHomeConfigRejectsInvalidPorts(t *testing.T) {
	for _, address := range []string{"home:abc", "home:0", "home:65536", "https://home:443", "home:-1"} {
		t.Run(address, func(t *testing.T) {
			c := HomeConfig{Enabled: true, Address: address, ServerName: "home", CAFile: "ca.pem", CertFile: "cert.pem", KeyFile: "key.pem", APIKey: "configured"}
			require.Error(t, c.Validate())
		})
	}
}
