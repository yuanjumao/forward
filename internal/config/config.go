package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server      ServerConfig      `yaml:"server"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
	Strategy    string            `yaml:"strategy"` // "order"（默认）或 "balance"
	Fallback    string            `yaml:"fallback"` // "failover"（默认，自动切换到可用模型）或 "failed"（直接报错）
	Backends    []BackendConfig   `yaml:"backends"`
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
}

type HealthCheckConfig struct {
	Interval int    `yaml:"interval"` // 健康检查间隔（秒）
	Timeout  int    `yaml:"timeout"`  // 单次检查超时（秒）
	Message  string `yaml:"message"`  // 健康检查发送的消息
	Expected string `yaml:"expected"` // 期望返回（参考用）
}

type BackendConfig struct {
	Name    string   `yaml:"name"`
	BaseURL string   `yaml:"base_url"`
	APIKey  string   `yaml:"api_key"`
	Models  []string `yaml:"models"` // 该后端支持的模型列表
	Model   string   `yaml:"model"`  // 兼容单模型配置
}

// EffectiveModels 返回该后端实际支持的模型列表
func (b *BackendConfig) EffectiveModels() []string {
	if len(b.Models) > 0 {
		return b.Models
	}
	if b.Model != "" {
		return []string{b.Model}
	}
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// 设置默认值
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}
	if cfg.HealthCheck.Interval <= 0 {
		cfg.HealthCheck.Interval = 60
	}
	if cfg.HealthCheck.Timeout <= 0 {
		cfg.HealthCheck.Timeout = 30
	}
	if cfg.HealthCheck.Message == "" {
		cfg.HealthCheck.Message = "hello"
	}
	if cfg.HealthCheck.Expected == "" {
		cfg.HealthCheck.Expected = "hello"
	}

	// 策略默认值与校验
	switch cfg.Strategy {
	case "":
		cfg.Strategy = "order"
	case "order", "balance":
		// ok
	default:
		return nil, fmt.Errorf("不支持的 strategy %q，可选值: order, balance", cfg.Strategy)
	}

	// fallback 默认值与校验
	switch cfg.Fallback {
	case "":
		cfg.Fallback = "failover"
	case "failover", "failed":
		// ok
	default:
		return nil, fmt.Errorf("不支持的 fallback %q，可选值: failover, failed", cfg.Fallback)
	}

	// 校验后端配置
	for i, b := range cfg.Backends {
		if b.Name == "" {
			return nil, fmt.Errorf("第 %d 个后端缺少 name", i+1)
		}
		if b.BaseURL == "" {
			return nil, fmt.Errorf("后端 %q 缺少 base_url", b.Name)
		}
		models := b.EffectiveModels()
		if len(models) == 0 {
			return nil, fmt.Errorf("后端 %q 缺少 model 或 models 配置", b.Name)
		}
	}

	return cfg, nil
}
