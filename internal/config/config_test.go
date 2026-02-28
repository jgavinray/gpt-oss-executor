package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes content to a file named "config.yaml" in dir and
// returns the full path.
func writeConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	return path
}

// minimalValidYAML is the smallest YAML that passes Validate after defaults
// are applied.
const minimalValidYAML = `
executor:
  gpt_oss_url: "http://gptoss.example.com"
  openclaw_gateway_url: "http://gateway.example.com"
  openclaw_gateway_token: "tok-abc"
`

// TestLoad covers file loading, YAML parse errors, validation failures, and
// default application.
func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		errContains string
		check       func(t *testing.T, cfg *Config)
	}{
		{
			name: "valid minimal YAML loads with defaults",
			yaml: minimalValidYAML,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Executor.GptOSSURL != "http://gptoss.example.com" {
					t.Errorf("GptOSSURL = %q, want %q", cfg.Executor.GptOSSURL, "http://gptoss.example.com")
				}
				// defaults should be applied
				if cfg.Executor.GptOSSModel != "gpt-oss" {
					t.Errorf("GptOSSModel = %q, want %q", cfg.Executor.GptOSSModel, "gpt-oss")
				}
				if cfg.Executor.MaxIterations != 5 {
					t.Errorf("MaxIterations = %d, want 5", cfg.Executor.MaxIterations)
				}
				if cfg.Executor.RunTimeoutSeconds != 300 {
					t.Errorf("RunTimeoutSeconds = %d, want 300", cfg.Executor.RunTimeoutSeconds)
				}
			},
		},
		{
			name: "missing gpt_oss_url returns error",
			yaml: `
executor:
  openclaw_gateway_url: "http://gateway.example.com"
  openclaw_gateway_token: "tok-abc"
`,
			wantErr:     true,
			errContains: "gpt_oss_url",
		},
		{
			name: "missing openclaw_gateway_url returns error",
			yaml: `
executor:
  gpt_oss_url: "http://gptoss.example.com"
  openclaw_gateway_token: "tok-abc"
`,
			wantErr:     true,
			errContains: "openclaw_gateway_url",
		},
		{
			name: "missing openclaw_gateway_token returns error",
			yaml: `
executor:
  gpt_oss_url: "http://gptoss.example.com"
  openclaw_gateway_url: "http://gateway.example.com"
`,
			wantErr:     true,
			errContains: "openclaw_gateway_token",
		},
		{
			name:        "invalid YAML syntax returns parse error",
			yaml:        "executor: [\nbad yaml",
			wantErr:     true,
			errContains: "unmarshalling YAML",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := writeConfig(t, dir, tc.yaml)

			cfg, err := Load(path)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

// TestLoad_FileNotFound verifies that Load returns an error containing the
// path when the config file does not exist.
func TestLoad_FileNotFound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.yaml")

	_, err := Load(missing)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not contain path %q", err.Error(), missing)
	}
}

// TestLoad_EnvOverrides verifies that environment variables take precedence
// over values in the YAML file.
//
// Note: subtests that call t.Setenv must NOT also call t.Parallel — Go's
// testing package enforces this constraint at runtime. The parent test is
// therefore also not marked parallel so the environment mutations are safe.
func TestLoad_EnvOverrides(t *testing.T) {
	tests := []struct {
		name   string
		envKey string
		envVal string
		yaml   string
		check  func(t *testing.T, cfg *Config)
	}{
		{
			name:   "GPTOSS_EXECUTOR_GATEWAY_TOKEN overrides gateway_token",
			envKey: "GPTOSS_EXECUTOR_GATEWAY_TOKEN",
			envVal: "env-token-xyz",
			yaml:   minimalValidYAML,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Executor.OpenClawGatewayToken != "env-token-xyz" {
					t.Errorf("OpenClawGatewayToken = %q, want %q", cfg.Executor.OpenClawGatewayToken, "env-token-xyz")
				}
			},
		},
		{
			name:   "GPTOSS_EXECUTOR_GPT_OSS_URL overrides gpt_oss_url",
			envKey: "GPTOSS_EXECUTOR_GPT_OSS_URL",
			envVal: "http://env-gptoss.example.com",
			yaml:   minimalValidYAML,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Executor.GptOSSURL != "http://env-gptoss.example.com" {
					t.Errorf("GptOSSURL = %q, want %q", cfg.Executor.GptOSSURL, "http://env-gptoss.example.com")
				}
			},
		},
		{
			name:   "GPTOSS_EXECUTOR_PORT overrides http_server.port",
			envKey: "GPTOSS_EXECUTOR_PORT",
			envVal: "9090",
			yaml:   minimalValidYAML,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.HTTPServer.Port != 9090 {
					t.Errorf("HTTPServer.Port = %d, want 9090", cfg.HTTPServer.Port)
				}
			},
		},
		{
			name:   "GPTOSS_EXECUTOR_LOG_LEVEL overrides logging.level",
			envKey: "GPTOSS_EXECUTOR_LOG_LEVEL",
			envVal: "debug",
			yaml:   minimalValidYAML,
			check: func(t *testing.T, cfg *Config) {
				t.Helper()
				if cfg.Logging.Level != "debug" {
					t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "debug")
				}
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		// t.Parallel is intentionally omitted here: t.Setenv requires the
		// subtest and its parent to run sequentially.
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.envKey, tc.envVal)

			dir := t.TempDir()
			path := writeConfig(t, dir, tc.yaml)

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

// TestLoad_Defaults verifies that applyDefaults fills in every zero-value
// field when a minimal YAML is loaded.
func TestLoad_Defaults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writeConfig(t, dir, minimalValidYAML)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tests := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"GptOSSModel defaults to gpt-oss", cfg.Executor.GptOSSModel, "gpt-oss"},
		{"MaxIterations defaults to 5", cfg.Executor.MaxIterations, 5},
		{"RunTimeoutSeconds defaults to 300", cfg.Executor.RunTimeoutSeconds, 300},
		{"Parser.Strategy defaults to react", cfg.Parser.Strategy, "react"},
		{"Parser.SourceField defaults to reasoning", cfg.Parser.SourceField, "reasoning"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("got %v, want %v", tc.got, tc.want)
			}
		})
	}
}

// TestConfig_SystemPrompt exercises the SystemPrompt method.
func TestConfig_SystemPrompt(t *testing.T) {
	t.Parallel()

	t.Run("empty SystemPromptPath returns empty string and no error", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{}
		got, err := cfg.SystemPrompt()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("valid path returns file contents", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		promptFile := filepath.Join(dir, "prompt.txt")
		const content = "You are a helpful assistant."
		if err := os.WriteFile(promptFile, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg := &Config{}
		cfg.Parser.SystemPromptPath = promptFile

		got, err := cfg.SystemPrompt()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != content {
			t.Errorf("got %q, want %q", got, content)
		}
	})

	t.Run("non-existent path returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		cfg := &Config{}
		cfg.Parser.SystemPromptPath = filepath.Join(dir, "no-such-file.txt")

		_, err := cfg.SystemPrompt()
		if err == nil {
			t.Fatal("expected error for missing prompt file, got nil")
		}
	})
}

// TestConfig_GuidedJSONSchema exercises the GuidedJSONSchema method.
func TestConfig_GuidedJSONSchema(t *testing.T) {
	t.Parallel()

	t.Run("empty path returns nil and no error", func(t *testing.T) {
		t.Parallel()
		cfg := &Config{}
		got, err := cfg.GuidedJSONSchema()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("valid JSON file returns parsed map", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		schemaFile := filepath.Join(dir, "schema.json")
		schemaContent := `{"type":"object","properties":{"name":{"type":"string"}}}`
		if err := os.WriteFile(schemaFile, []byte(schemaContent), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg := &Config{}
		cfg.Parser.GuidedJSONSchemaPath = schemaFile

		got, err := cfg.GuidedJSONSchema()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("got nil map, want non-nil")
		}

		// Verify the parsed content round-trips correctly.
		var want map[string]interface{}
		if err := json.Unmarshal([]byte(schemaContent), &want); err != nil {
			t.Fatalf("json.Unmarshal reference: %v", err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("schema mismatch:\n got  %s\n want %s", gotJSON, wantJSON)
		}
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		schemaFile := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(schemaFile, []byte("{not valid json}"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		cfg := &Config{}
		cfg.Parser.GuidedJSONSchemaPath = schemaFile

		_, err := cfg.GuidedJSONSchema()
		if err == nil {
			t.Fatal("expected error for invalid JSON, got nil")
		}
	})
}
