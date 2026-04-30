package pool

import (
	"ai-proxy/internal/config"

	openai "github.com/sashabaranov/go-openai"
)

// ClientPool 预创建并缓存每个后端的 openai.Client，复用底层 HTTP 连接
type ClientPool struct {
	clients map[string]*openai.Client // key: backend name
}

// New 根据配置为每个后端创建一个 client
func New(cfg *config.Config) *ClientPool {
	p := &ClientPool{
		clients: make(map[string]*openai.Client, len(cfg.Backends)),
	}

	for _, b := range cfg.Backends {
		clientCfg := openai.DefaultConfig(b.APIKey)
		clientCfg.BaseURL = b.BaseURL
		p.clients[b.Name] = openai.NewClientWithConfig(clientCfg)
	}

	return p
}

// Get 获取指定后端的 client
func (p *ClientPool) Get(backendName string) *openai.Client {
	return p.clients[backendName]
}
