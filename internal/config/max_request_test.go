package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestProxyMaxRequest_DefaultAndExplicit(t *testing.T) {
	var p ProxyConfig
	if got := p.MaxRequest(); got != DefaultMaxRequest {
		t.Errorf("default MaxRequest = %s, want %s", got, DefaultMaxRequest)
	}
	p.MaxRequestSeconds = 45
	if got := p.MaxRequest(); got != 45*time.Second {
		t.Errorf("explicit MaxRequest = %s, want 45s", got)
	}
}

func TestTimeoutPreviouslyUnenforced(t *testing.T) {
	unenforced := []AdapterType{AdapterClaudeCLI, AdapterCodexCLI, AdapterCodexSubagent, AdapterGeminiCLI, AdapterBedrockAPI}
	for _, a := range unenforced {
		if !timeoutPreviouslyUnenforced(a) {
			t.Errorf("%s should be flagged as previously ignoring timeout_seconds", a)
		}
	}
	// An adapter type added later must not be reported as previously unbounded.
	for _, a := range []AdapterType{AdapterOllamaHTTP, AdapterAnthropicAPI, AdapterOpenAIAPI, AdapterType("future_adapter")} {
		if timeoutPreviouslyUnenforced(a) {
			t.Errorf("%s must not be flagged as previously unbounded", a)
		}
	}
}

// The warning text is derived from DefaultBackendTimeout, the same constant
// Timeout() falls back to, so the two cannot drift apart.
func TestTimeoutWarning_DerivedFromDefaultConstant(t *testing.T) {
	var b BackendConfig
	if b.Timeout() != DefaultBackendTimeout {
		t.Fatalf("Timeout() = %s, want DefaultBackendTimeout %s", b.Timeout(), DefaultBackendTimeout)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	c := &Config{Backends: map[string]*BackendConfig{"cli": {Adapter: AdapterClaudeCLI}}}
	if err := c.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(buf.String(), DefaultBackendTimeout.String()) {
		t.Errorf("warning does not carry %q: %s", DefaultBackendTimeout, buf.String())
	}
}
