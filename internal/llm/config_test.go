package llm

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveDefaultsAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantURL string
		wantErr bool
	}{
		{
			name:    "a named https endpoint with a key",
			config:  Config{Provider: "gw", Model: "m", APIKey: "k", BaseURL: "https://gw.example.com/v1/"},
			wantURL: "https://gw.example.com/v1",
		},
		{
			name:    "a loopback endpoint needs no key",
			config:  Config{Model: "m", BaseURL: "http://127.0.0.1:8080/v1"},
			wantURL: "http://127.0.0.1:8080/v1",
		},
		{name: "no model", config: Config{APIKey: "k", BaseURL: "https://gw.example.com/v1"}, wantErr: true},
		{name: "no base url", config: Config{Model: "m", APIKey: "k"}, wantErr: true},
		{name: "a malformed base url", config: Config{Model: "m", APIKey: "k", BaseURL: "not a url"}, wantErr: true},
		{name: "plaintext to a remote host", config: Config{Model: "m", APIKey: "k", BaseURL: "http://gw.example.com/v1"}, wantErr: true},
		{name: "a remote host without a key", config: Config{Model: "m", BaseURL: "https://gw.example.com/v1"}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolved, err := test.config.Resolve()

			if test.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if resolved.BaseURL != test.wantURL {
				t.Errorf("base URL = %q, want %q", resolved.BaseURL, test.wantURL)
			}
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
		if _, err := (Config{Model: "m", BaseURL: url}).Resolve(); err != nil {
			t.Errorf("%s was not treated as loopback: %v", url, err)
		}
	}

	for _, url := range []string{
		"http://10.0.0.5/v1",
		"http://[2001:db8::1]/v1",
		"http://localhost.example.com/v1",
	} {
		if _, err := (Config{Model: "m", APIKey: "k", BaseURL: url}).Resolve(); err == nil {
			t.Errorf("%s was accepted over plaintext", url)
		}
	}
}

func TestAMissingKeyNamesTheProviderAndTheHost(t *testing.T) {
	_, err := (Config{Provider: "acme", Model: "m", BaseURL: "https://gw.example.com/v1"}).Resolve()

	if !errors.Is(err, ErrMissingCredential) {
		t.Fatalf("err = %v, want ErrMissingCredential", err)
	}

	for _, want := range []string{"acme", "gw.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}
