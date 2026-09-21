package provider_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/provider"
)

func TestResolveDefaultsAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  provider.ClientConfig
		wantURL string
		wantErr bool
	}{
		{
			name:    "a named https endpoint with a key",
			config:  provider.ClientConfig{Provider: "gw", Model: "m", APIKey: "k", BaseURL: "https://gw.example.com/v1/"},
			wantURL: litHTTPSGwExampleCom,
		},
		{
			name:    "a loopback endpoint needs no key",
			config:  provider.ClientConfig{Model: "m", BaseURL: "http://127.0.0.1:8080/v1"},
			wantURL: "http://127.0.0.1:8080/v1",
		},
		{name: "no model", config: provider.ClientConfig{APIKey: "k", BaseURL: litHTTPSGwExampleCom}, wantErr: true},
		{name: "no base url", config: provider.ClientConfig{Model: "m", APIKey: "k"}, wantErr: true},
		{name: "a malformed base url", config: provider.ClientConfig{Model: "m", APIKey: "k", BaseURL: "not a url"}, wantErr: true},
		{name: "plaintext to a remote host", config: provider.ClientConfig{Model: "m", APIKey: "k", BaseURL: "http://gw.example.com/v1"}, wantErr: true},
		{name: "a remote host without a key", config: provider.ClientConfig{Model: "m", BaseURL: litHTTPSGwExampleCom}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := test.config.Resolve()

			if test.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			assert.Equal(t, test.wantURL, resolved.BaseURL)
		})
	}
}

func TestLoopbackIsRecognisedInEveryForm(t *testing.T) {
	for _, url := range []string{
		"http://localhost:8080/v1",
		"http://LOCALHOST/v1",
		"http://127.0.0.1/v1",
		"http://127.1.2.3:9/v1",
		"http://[::1]:8080/v1",
		"http://[::1]/v1",
	} {
		_, err := (provider.ClientConfig{Model: "m", BaseURL: url}).Resolve()
		require.NoError(t, err, "%s was not treated as loopback", url)
	}

	for _, url := range []string{
		"http://10.0.0.5/v1",
		"http://[2001:db8::1]/v1",
		"http://localhost.example.com/v1",
	} {
		_, err := (provider.ClientConfig{Model: "m", APIKey: "k", BaseURL: url}).Resolve()
		require.Error(t, err, "%s was accepted over plaintext", url)
	}
}

func TestAMissingKeyNamesTheProviderAndTheHost(t *testing.T) {
	_, err := (provider.ClientConfig{Provider: "acme", Model: "m", BaseURL: litHTTPSGwExampleCom}).Resolve()

	require.ErrorIs(t, err, provider.ErrMissingCredential)

	for _, want := range []string{"acme", "gw.example.com"} {
		assert.Contains(t, err.Error(), want)
	}
}
