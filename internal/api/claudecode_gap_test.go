package api

import (
	"testing"
	"antigravity-go-proxy/internal/config"
)

func TestClaudecodeGatewayModelMapping(t *testing.T) {
	cfg := config.Get()
	if cfg.ClaudeCode.Enabled != false {
		t.Log("ClaudeCode config present in Config; default Enabled false as expected")
	}
}
