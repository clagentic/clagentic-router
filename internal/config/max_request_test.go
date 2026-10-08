package config

import (
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
	for _, a := range []AdapterType{AdapterOllamaHTTP, AdapterAnthropicAPI, AdapterOpenAIAPI} {
		if timeoutPreviouslyUnenforced(a) {
			t.Errorf("%s already enforced timeout_seconds", a)
		}
	}
}
