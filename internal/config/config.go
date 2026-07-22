// Package config loads Miranda's YAML configuration file and merges it over
// built-in defaults, so the agent runs with sane behavior even with an empty
// or partial config.yaml.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root of the agent's configuration tree. Every field must have
// a value set in Default() so a missing or partial config.yaml still yields a
// fully runnable configuration.
type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
	LLM     LLMConfig     `yaml:"llm"`
	Memory  MemoryConfig  `yaml:"memory"`
	MCP     MCPConfig     `yaml:"mcp"`
	TTS     TTSConfig     `yaml:"tts"`
	WebUI   WebUIConfig   `yaml:"web_ui"`
}

// ServerConfig controls the unified command interface / web UI HTTP server.
type ServerConfig struct {
	HTTPAddr  string `yaml:"http_addr"`
	AuthToken string `yaml:"auth_token"`
}

// StorageConfig points at the on-disk SQLite history DB and markdown memory dir.
type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
	MemoryDir  string `yaml:"memory_dir"`
}

// LLMProvider describes one configured model backend, either an
// OpenAI-compatible endpoint (local or hosted) or native Anthropic.
type LLMProvider struct {
	Name      string `yaml:"name"`
	Type      string `yaml:"type"` // "openai_compat" | "anthropic"
	BaseURL   string `yaml:"base_url,omitempty"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env,omitempty"`
}

// EscalationConfig configures the explicit escalate_to_claude-style tool that
// lets a cheap/local model hand off a hard turn to a stronger provider.
type EscalationConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ToolName       string `yaml:"tool_name"`
	TargetProvider string `yaml:"target_provider"`
}

// LLMConfig is the ordered provider fallback chain plus escalation settings.
type LLMConfig struct {
	// Providers is tried in order on error/timeout of the previous one.
	Providers       []LLMProvider    `yaml:"providers"`
	DefaultProvider string           `yaml:"default_provider"`
	Escalation      EscalationConfig `yaml:"escalation"`
}

// MemoryConfig controls how per-user markdown memory gets updated.
type MemoryConfig struct {
	AutoSummarize bool `yaml:"auto_summarize"`
	ExplicitTool  bool `yaml:"explicit_tool"`
}

// MCPServer is one MCP server the agent connects to as a tool source.
type MCPServer struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"token_env,omitempty"`
	Enabled  bool   `yaml:"enabled"`
}

// MCPConfig lists the MCP servers (HA + others) available as tool sources.
type MCPConfig struct {
	Servers []MCPServer `yaml:"servers"`
}

// YandexStationConfig configures the primary TTS channel.
type YandexStationConfig struct {
	Entities           []string `yaml:"entities"`
	ChunkMaxChars      int      `yaml:"chunk_max_chars"`
	IdlePollIntervalMS int      `yaml:"idle_poll_interval_ms"`
}

// HATTSConfig configures the optional Home Assistant native TTS fallback.
type HATTSConfig struct {
	Enabled      bool   `yaml:"enabled"`
	EntityID     string `yaml:"entity_id"`
	TargetPlayer string `yaml:"target_player"`
}

// TTSConfig selects and configures the primary/fallback TTS channels.
type TTSConfig struct {
	Primary       string              `yaml:"primary"`
	YandexStation YandexStationConfig `yaml:"yandex_station"`
	Fallback      string              `yaml:"fallback"`
	HATTS         HATTSConfig         `yaml:"ha_tts"`
}

// WebUIConfig controls the embedded monitoring dashboard.
type WebUIConfig struct {
	Enabled       bool `yaml:"enabled"`
	LogBufferSize int  `yaml:"log_buffer_size"`
}

// Default returns the built-in configuration used when config.yaml is absent
// or only overrides a subset of fields.
func Default() Config {
	return Config{
		Server: ServerConfig{
			HTTPAddr:  ":8787",
			AuthToken: "",
		},
		Storage: StorageConfig{
			SQLitePath: "./data/miranda.db",
			MemoryDir:  "./data/memory",
		},
		LLM: LLMConfig{
			Providers:       nil,
			DefaultProvider: "",
			Escalation: EscalationConfig{
				Enabled:        true,
				ToolName:       "escalate_to_claude",
				TargetProvider: "claude",
			},
		},
		Memory: MemoryConfig{
			AutoSummarize: true,
			ExplicitTool:  true,
		},
		MCP: MCPConfig{
			Servers: nil,
		},
		TTS: TTSConfig{
			Primary: "yandex_station",
			YandexStation: YandexStationConfig{
				Entities:           nil,
				ChunkMaxChars:      100,
				IdlePollIntervalMS: 300,
			},
			Fallback: "",
			HATTS: HATTSConfig{
				Enabled: false,
			},
		},
		WebUI: WebUIConfig{
			Enabled:       true,
			LogBufferSize: 1000,
		},
	}
}

// Load reads the YAML file at path and merges it over Default(). A missing
// file is not an error: it just means the defaults are used as-is, so the
// agent has a working config out of the box.
//
// yaml.Unmarshal only overwrites fields present in the document, so starting
// from a fully-populated default struct gives us a cheap partial merge
// without a separate deep-merge step.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}

	return cfg, nil
}
