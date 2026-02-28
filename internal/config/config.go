package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure.
type Config struct {
	Executor   ExecutorConfig   `yaml:"executor"`
	Parser     ParserConfig     `yaml:"parser"`
	HTTPServer HTTPServerConfig `yaml:"http_server"`
	Logging    LoggingConfig    `yaml:"logging"`
	Tools      ToolsConfig      `yaml:"tools"`
}

// ExecutorConfig holds agentic loop and LLM connection settings.
type ExecutorConfig struct {
	GptOSSURL                string  `yaml:"gpt_oss_url"`
	GptOSSModel              string  `yaml:"gpt_oss_model"`
	GptOSSTemperature        float32 `yaml:"gpt_oss_temperature"`
	GptOSSMaxTokens          int     `yaml:"gpt_oss_max_tokens"`
	GptOSSCallTimeoutSeconds int     `yaml:"gpt_oss_call_timeout_seconds"`
	MaxIterations            int     `yaml:"max_iterations"`
	MaxRetries               int     `yaml:"max_retries"`
	RunTimeoutSeconds        int     `yaml:"run_timeout_seconds"`
	ContextWindowLimit       int     `yaml:"context_window_limit"`
	ContextBufferTokens      int     `yaml:"context_buffer_tokens"`
	ContextCompactThreshold  float64 `yaml:"context_compact_threshold"`
	ContextTruncThreshold    float64 `yaml:"context_trunc_threshold"`
	OpenClawGatewayURL       string  `yaml:"openclaw_gateway_url"`
	OpenClawGatewayToken     string  `yaml:"openclaw_gateway_token"`
	OpenClawSessionKey       string  `yaml:"openclaw_session_key"`
}

// ParserConfig holds response parsing strategy settings.
type ParserConfig struct {
	Strategy              string `yaml:"strategy"`
	FallbackStrategy      string `yaml:"fallback_strategy"`
	SourceField           string `yaml:"source_field"`
	FallbackField         string `yaml:"fallback_field"`
	SystemPromptPath      string `yaml:"system_prompt_path"`
	GuidedJSONSchemaPath  string `yaml:"guided_json_schema_path"`
}

// HTTPServerConfig holds HTTP server listen settings.
type HTTPServerConfig struct {
	Port                  int    `yaml:"port"`
	Bind                  string `yaml:"bind"`
	ReadTimeoutSeconds    int    `yaml:"read_timeout_seconds"`
	WriteTimeoutSeconds   int    `yaml:"write_timeout_seconds"`
	IdleTimeoutSeconds    int    `yaml:"idle_timeout_seconds"`
	ShutdownTimeoutSeconds int   `yaml:"shutdown_timeout_seconds"`
}

// LoggingConfig holds structured logging settings.
type LoggingConfig struct {
	Level            string `yaml:"level"`
	Format           string `yaml:"format"`
	Output           string `yaml:"output"`
	ErrorLogDir      string `yaml:"error_log_dir"`
	ErrorLogFilename string `yaml:"error_log_filename"`
}

// ToolsConfig holds tool enablement and per-tool settings.
type ToolsConfig struct {
	Enabled               []string       `yaml:"enabled"`
	DefaultTimeoutSeconds int            `yaml:"default_timeout_seconds"`
	ResultLimits          map[string]int `yaml:"result_limits"`
	WebSearch             WebSearchConfig `yaml:"web_search"`
	WebFetch              WebFetchConfig  `yaml:"web_fetch"`
	Read                  ReadConfig      `yaml:"read"`
	Write                 WriteConfig     `yaml:"write"`
	Exec                  ExecConfig      `yaml:"exec"`
	Browser               BrowserConfig   `yaml:"browser"`
}

// WebSearchConfig holds web_search tool settings.
type WebSearchConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"`
	MaxResults     int `yaml:"max_results"`
}

// WebFetchConfig holds web_fetch tool settings.
type WebFetchConfig struct {
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	MaxChars       int    `yaml:"max_chars"`
	ExtractMode    string `yaml:"extract_mode"`
}

// ReadConfig holds read tool settings.
type ReadConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// WriteConfig holds write tool settings.
type WriteConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// ExecConfig holds exec tool settings.
type ExecConfig struct {
	TimeoutSeconds  int      `yaml:"timeout_seconds"`
	BlockedCommands []string `yaml:"blocked_commands"`
}

// BrowserConfig holds browser tool settings.
type BrowserConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds"`
}

// Load reads the YAML file at path, expands ${ENV_VAR} references in values,
// unmarshals into Config, applies environment variable overrides, sets defaults
// for any zero-value fields, and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading file %q: %w", path, err)
	}

	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("config: unmarshalling YAML: %w", err)
	}

	applyEnvOverrides(&cfg)
	applyDefaults(&cfg)

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: validation: %w", err)
	}

	return &cfg, nil
}

// applyEnvOverrides overwrites specific Config fields when the corresponding
// environment variables are set.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("GPTOSS_EXECUTOR_GPT_OSS_URL"); v != "" {
		cfg.Executor.GptOSSURL = v
	}
	if v := os.Getenv("GPTOSS_EXECUTOR_GATEWAY_URL"); v != "" {
		cfg.Executor.OpenClawGatewayURL = v
	}
	if v := os.Getenv("GPTOSS_EXECUTOR_GATEWAY_TOKEN"); v != "" {
		cfg.Executor.OpenClawGatewayToken = v
	}
	if v := os.Getenv("GPTOSS_EXECUTOR_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.HTTPServer.Port = port
		}
	}
	if v := os.Getenv("GPTOSS_EXECUTOR_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
}

// applyDefaults sets zero-value fields to their documented defaults.
func applyDefaults(cfg *Config) {
	// Executor defaults
	if cfg.Executor.GptOSSModel == "" {
		cfg.Executor.GptOSSModel = "gpt-oss"
	}
	if cfg.Executor.GptOSSTemperature == 0 {
		cfg.Executor.GptOSSTemperature = 0.25
	}
	if cfg.Executor.GptOSSMaxTokens == 0 {
		cfg.Executor.GptOSSMaxTokens = 1000
	}
	if cfg.Executor.MaxIterations == 0 {
		cfg.Executor.MaxIterations = 5
	}
	if cfg.Executor.MaxRetries == 0 {
		cfg.Executor.MaxRetries = 3
	}
	if cfg.Executor.RunTimeoutSeconds == 0 {
		cfg.Executor.RunTimeoutSeconds = 300
	}
	if cfg.Executor.ContextWindowLimit == 0 {
		cfg.Executor.ContextWindowLimit = 32768
	}
	if cfg.Executor.ContextBufferTokens == 0 {
		cfg.Executor.ContextBufferTokens = 2000
	}
	if cfg.Executor.ContextCompactThreshold == 0 {
		cfg.Executor.ContextCompactThreshold = 0.8
	}
	if cfg.Executor.ContextTruncThreshold == 0 {
		cfg.Executor.ContextTruncThreshold = 0.6
	}
	if cfg.Executor.OpenClawSessionKey == "" {
		cfg.Executor.OpenClawSessionKey = "main"
	}

	// Parser defaults
	if cfg.Parser.Strategy == "" {
		cfg.Parser.Strategy = "react"
	}
	if cfg.Parser.FallbackStrategy == "" {
		cfg.Parser.FallbackStrategy = "fuzzy"
	}
	if cfg.Parser.SourceField == "" {
		cfg.Parser.SourceField = "reasoning"
	}
	if cfg.Parser.FallbackField == "" {
		cfg.Parser.FallbackField = "content"
	}

	// HTTPServer defaults
	if cfg.HTTPServer.Port == 0 {
		cfg.HTTPServer.Port = 8001
	}
	if cfg.HTTPServer.Bind == "" {
		cfg.HTTPServer.Bind = "127.0.0.1"
	}

	// Logging defaults
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = "json"
	}
	if cfg.Logging.Output == "" {
		cfg.Logging.Output = "stdout"
	}
}

// Validate returns an error if required fields are missing or values are out
// of range.
func (c *Config) Validate() error {
	if c.Executor.GptOSSURL == "" {
		return fmt.Errorf("executor.gpt_oss_url is required")
	}
	if c.Executor.OpenClawGatewayURL == "" {
		return fmt.Errorf("executor.openclaw_gateway_url is required")
	}
	if c.Executor.OpenClawGatewayToken == "" {
		return fmt.Errorf("executor.openclaw_gateway_token is required (set GPTOSS_EXECUTOR_GATEWAY_TOKEN)")
	}
	if c.Executor.MaxIterations < 1 {
		return fmt.Errorf("executor.max_iterations must be >= 1, got %d", c.Executor.MaxIterations)
	}
	if c.Executor.RunTimeoutSeconds < 1 {
		return fmt.Errorf("executor.run_timeout_seconds must be >= 1, got %d", c.Executor.RunTimeoutSeconds)
	}
	return nil
}

// SystemPrompt reads and returns the contents of Parser.SystemPromptPath.
// If SystemPromptPath is empty, it returns an empty string and no error.
func (c *Config) SystemPrompt() (string, error) {
	if c.Parser.SystemPromptPath == "" {
		return "", nil
	}
	data, err := os.ReadFile(c.Parser.SystemPromptPath)
	if err != nil {
		return "", fmt.Errorf("config: reading system prompt %q: %w", c.Parser.SystemPromptPath, err)
	}
	return string(data), nil
}

// GuidedJSONSchema reads and parses the JSON file at Parser.GuidedJSONSchemaPath.
// If GuidedJSONSchemaPath is empty, it returns nil and no error.
func (c *Config) GuidedJSONSchema() (map[string]interface{}, error) {
	if c.Parser.GuidedJSONSchemaPath == "" {
		return nil, nil
	}
	data, err := os.ReadFile(c.Parser.GuidedJSONSchemaPath)
	if err != nil {
		return nil, fmt.Errorf("config: reading guided JSON schema %q: %w", c.Parser.GuidedJSONSchemaPath, err)
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("config: parsing guided JSON schema %q: %w", c.Parser.GuidedJSONSchemaPath, err)
	}
	return schema, nil
}
