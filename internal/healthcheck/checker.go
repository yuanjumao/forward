package healthcheck

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"ai-proxy/internal/config"

	openai "github.com/sashabaranov/go-openai"
)

// BackendStatus 记录单个后端某个模型的状态
type BackendStatus struct {
	BackendName string    `json:"backend_name"`
	Model       string    `json:"model"`
	BaseURL     string    `json:"base_url"`
	Healthy     bool      `json:"healthy"`
	LastCheck   time.Time `json:"last_check"`
	LastError   string    `json:"last_error,omitempty"`
}

// statusKey 用于标识一个后端+模型的组合
func statusKey(backendName, model string) string {
	return backendName + "::" + model
}

// Checker 健康检查器
type Checker struct {
	cfg      *config.Config
	mu       sync.RWMutex
	statuses map[string]*BackendStatus // key: "backendName::model"
	stopCh   chan struct{}
}

func New(cfg *config.Config) *Checker {
	c := &Checker{
		cfg:      cfg,
		statuses: make(map[string]*BackendStatus),
		stopCh:   make(chan struct{}),
	}

	// 初始化所有后端+模型的状态
	for _, b := range cfg.Backends {
		for _, model := range b.EffectiveModels() {
			key := statusKey(b.Name, model)
			c.statuses[key] = &BackendStatus{
				BackendName: b.Name,
				Model:       model,
				BaseURL:     b.BaseURL,
				Healthy:     false,
			}
		}
	}

	return c
}

// CheckAll 对所有后端的所有模型执行健康检查
func (c *Checker) CheckAll() {
	var wg sync.WaitGroup
	for _, b := range c.cfg.Backends {
		for _, model := range b.EffectiveModels() {
			wg.Add(1)
			go func(backend config.BackendConfig, m string) {
				defer wg.Done()
				c.checkOne(backend, m)
			}(b, model)
		}
	}
	wg.Wait()
}

// checkOne 使用 go-openai 库对单个后端的单个模型执行健康检查
func (c *Checker) checkOne(backend config.BackendConfig, model string) {
	key := statusKey(backend.Name, model)
	msg := c.cfg.HealthCheck.Message
	expected := c.cfg.HealthCheck.Expected

	clientCfg := openai.DefaultConfig(backend.APIKey)
	clientCfg.BaseURL = backend.BaseURL
	client := openai.NewClientWithConfig(clientCfg)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.cfg.HealthCheck.Timeout)*time.Second)
	defer cancel()

	resp, err := client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("%s\nPlease reply with only \"%s\" and nothing else.", msg, expected),
			},
		},
		MaxTokens:   10,
		Temperature: 0,
	})

	if err != nil {
		c.setStatus(key, false, fmt.Sprintf("请求失败: %v", err))
		return
	}

	if len(resp.Choices) == 0 {
		c.setStatus(key, false, "响应中没有 choices")
		return
	}

	// 只要能正常返回就视为健康
	c.setStatus(key, true, "")
	log.Printf("[健康检查] ✓ %s/%s 正常", backend.Name, model)
}

func (c *Checker) setStatus(key string, healthy bool, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if s, ok := c.statuses[key]; ok {
		wasHealthy := s.Healthy
		s.Healthy = healthy
		s.LastCheck = time.Now()
		s.LastError = lastErr

		if !healthy {
			log.Printf("[健康检查] ✗ %s/%s 不可用: %s", s.BackendName, s.Model, lastErr)
		} else if !wasHealthy {
			log.Printf("[健康检查] ✓ %s/%s 恢复可用", s.BackendName, s.Model)
		}
	}
}

// MarkUnhealthy 在转发请求失败时立即标记后端不健康
func (c *Checker) MarkUnhealthy(backendName, model, reason string) {
	key := statusKey(backendName, model)
	c.setStatus(key, false, reason)
}

// Start 启动定时健康检查
func (c *Checker) Start() {
	interval := time.Duration(c.cfg.HealthCheck.Interval) * time.Second
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				log.Println("[健康检查] 执行定时检查...")
				c.CheckAll()
			case <-c.stopCh:
				return
			}
		}
	}()
	log.Printf("[健康检查] 定时检查已启动，间隔 %v", interval)
}

// Stop 停止定时健康检查
func (c *Checker) Stop() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
}

// HealthyBackends 返回当前所有健康的后端
func (c *Checker) HealthyBackends() []config.BackendConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	seen := make(map[string]bool)
	var result []config.BackendConfig
	for _, b := range c.cfg.Backends {
		for _, model := range b.EffectiveModels() {
			key := statusKey(b.Name, model)
			if s, ok := c.statuses[key]; ok && s.Healthy && !seen[b.Name] {
				seen[b.Name] = true
				result = append(result, b)
			}
		}
	}
	return result
}

// BackendsForModel 返回支持指定模型的所有健康后端（按配置顺序，用于 fallback）
func (c *Checker) BackendsForModel(model string) []BackendWithModel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var result []BackendWithModel
	for _, b := range c.cfg.Backends {
		for _, m := range b.EffectiveModels() {
			if m == model {
				key := statusKey(b.Name, model)
				if s, ok := c.statuses[key]; ok && s.Healthy {
					result = append(result, BackendWithModel{Backend: b, Model: model})
				}
				break
			}
		}
	}
	return result
}

// AllHealthyModels 返回所有健康的模型名称（去重）
func (c *Checker) AllHealthyModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	seen := make(map[string]bool)
	var result []string
	for _, s := range c.statuses {
		if s.Healthy && !seen[s.Model] {
			seen[s.Model] = true
			result = append(result, s.Model)
		}
	}
	return result
}

// AllStatuses 返回所有后端+模型的状态
func (c *Checker) AllStatuses() []BackendStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]BackendStatus, 0, len(c.statuses))
	for _, b := range c.cfg.Backends {
		for _, model := range b.EffectiveModels() {
			key := statusKey(b.Name, model)
			if s, ok := c.statuses[key]; ok {
				result = append(result, *s)
			}
		}
	}
	return result
}

// BackendWithModel 表示一个后端+模型的组合，用于自动选择
type BackendWithModel struct {
	Backend config.BackendConfig
	Model   string
}

// AllHealthyBackendModels 返回所有健康的后端+模型组合（按配置顺序）
func (c *Checker) AllHealthyBackendModels() []BackendWithModel {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var result []BackendWithModel
	for _, b := range c.cfg.Backends {
		for _, model := range b.EffectiveModels() {
			key := statusKey(b.Name, model)
			if s, ok := c.statuses[key]; ok && s.Healthy {
				result = append(result, BackendWithModel{Backend: b, Model: model})
			}
		}
	}
	return result
}

// HasAnyHealthy 是否有任何健康的后端
func (c *Checker) HasAnyHealthy() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, s := range c.statuses {
		if s.Healthy {
			return true
		}
	}
	return false
}
