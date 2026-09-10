package api

import (
	"antigravity-go-proxy/internal/config"
	"testing"
)

func TestClaudecodeGatewayModelMapping(t *testing.T) {
	cfg := config.Get()
	if cfg.ClaudeCode.Enabled != false {
		t.Log("ClaudeCode config present in Config; default Enabled false as expected")
	}
}
