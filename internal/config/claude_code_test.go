package config

import "testing"

func TestParseConfigBytesClaudeCodeModelListCloaking(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantCloak     bool
		wantDisable   bool
		wantEffective bool
	}{
		{
			name:          "defaults to canonical listing",
			yaml:          "port: 8317\n",
			wantCloak:     false,
			wantDisable:   false,
			wantEffective: false,
		},
		{
			name:          "legacy disable stays off",
			yaml:          "claude-code:\n  disable-cloaking-model-list: true\n",
			wantCloak:     false,
			wantDisable:   true,
			wantEffective: false,
		},
		{
			name:          "opt-in cloak-model-list",
			yaml:          "claude-code:\n  cloak-model-list: true\n",
			wantCloak:     true,
			wantDisable:   false,
			wantEffective: true,
		},
		{
			name:          "legacy disable wins over opt-in",
			yaml:          "claude-code:\n  cloak-model-list: true\n  disable-cloaking-model-list: true\n",
			wantCloak:     true,
			wantDisable:   true,
			wantEffective: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.ClaudeCode.CloakModelList; got != tt.wantCloak {
				t.Fatalf("CloakModelList = %t, want %t", got, tt.wantCloak)
			}
			if got := cfg.ClaudeCode.DisableCloakingModelList; got != tt.wantDisable {
				t.Fatalf("DisableCloakingModelList = %t, want %t", got, tt.wantDisable)
			}
			if got := cfg.ClaudeCode.CloakAnthropicListing(); got != tt.wantEffective {
				t.Fatalf("CloakAnthropicListing() = %t, want %t", got, tt.wantEffective)
			}
		})
	}
}
