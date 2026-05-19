package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"ai-proxy/internal/config"
	"ai-proxy/internal/healthcheck"
	"ai-proxy/internal/pool"

	openai "github.com/sashabaranov/go-openai"
)

type Handler struct {
	cfg     *config.Config
	checker *healthcheck.Checker
	pool    *pool.ClientPool
	counter uint64 // 用于 balance 模式的 round-robin 计数器
}

// OpenAI 兼容的错误响应
type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// 模型列表响应
type modelsResponse struct {
	Object string      `json:"object"`
	Data   []modelInfo `json:"data"`
}

type modelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// 状态响应
type statusResponse struct {
	Status   string                      `json:"status"`
	Strategy string                      `json:"strategy"`
	Fallback string                      `json:"fallback"`
	Healthy  int                         `json:"healthy_count"`
	Total    int                         `json:"total_count"`
	Backends []healthcheck.BackendStatus `json:"backends"`
}

func NewHandler(cfg *config.Config, checker *healthcheck.Checker, p *pool.ClientPool) *Handler {
	return &Handler{
		cfg:     cfg,
		checker: checker,
		pool:    p,
	}
}

// getClient 从连接池获取指定后端的 client（复用底层 TCP 连接）
func (h *Handler) getClient(backendName string) *openai.Client {
	return h.pool.Get(backendName)
}

// reorder 根据策略对候选列表排序
//
// order 模式：保持原始配置顺序，第一个可用就一直用第一个
// balance 模式：round-robin 轮转起始位置，请求均匀分布到各后端
func (h *Handler) reorder(candidates []healthcheck.BackendWithModel) []healthcheck.BackendWithModel {
	if len(candidates) <= 1 || h.cfg.Strategy == "order" {
		return candidates
	}

	// balance: round-robin
	n := uint64(len(candidates))
	idx := atomic.AddUint64(&h.counter, 1) % n

	reordered := make([]healthcheck.BackendWithModel, 0, len(candidates))
	reordered = append(reordered, candidates[idx:]...)
	reordered = append(reordered, candidates[:idx]...)
	return reordered
}

// ChatCompletions 处理 /v1/chat/completions 请求
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 方法")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	var req openai.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "解析请求体失败: "+err.Error())
		return
	}

	// 获取候选后端列表
	var candidates []healthcheck.BackendWithModel

	if req.Model != "" {
		candidates = h.checker.BackendsForModel(req.Model, "chat")
		if len(candidates) == 0 {
			// 指定的模型不可用，根据 fallback 策略决定行为
			if h.cfg.Fallback == "failed" {
				h.handleNoBackend(w, req.Model)
				return
			}
			// failover: 自动切换到其他可用模型
			candidates = h.checker.AllHealthyBackendModels("chat")
			if len(candidates) == 0 {
				writeError(w, http.StatusServiceUnavailable, "no_model_available",
					fmt.Sprintf("模型 %q 不可用，且没有任何其他可用的后端模型", req.Model))
				return
			}
			log.Printf("[代理] [failover] 模型 %q 不可用，自动切换到其他可用模型", req.Model)
		}
	} else {
		candidates = h.checker.AllHealthyBackendModels("chat")
		if len(candidates) == 0 {
			writeError(w, http.StatusServiceUnavailable, "no_model_available",
				"当前没有任何可用的后端模型")
			return
		}
	}

	// 根据策略排序
	candidates = h.reorder(candidates)

	if req.Stream {
		h.handleStream(w, r, req, candidates)
	} else {
		h.handleNonStream(w, req, candidates)
	}
}

// handleNonStream 非流式请求，逐个尝试候选后端，失败自动切换
func (h *Handler) handleNonStream(w http.ResponseWriter, req openai.ChatCompletionRequest, candidates []healthcheck.BackendWithModel) {
	var lastErr error

	for i, c := range candidates {
		req.Model = c.Model
		client := h.getClient(c.Backend.Name)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

		log.Printf("[代理] [%s] 尝试后端 %s/%s (%d/%d)", h.cfg.Strategy, c.Backend.Name, c.Model, i+1, len(candidates))

		resp, err := client.CreateChatCompletion(ctx, req)
		cancel()

		if err != nil {
			lastErr = err
			log.Printf("[代理] 后端 %s/%s 失败: %v", c.Backend.Name, c.Model, err)
			h.checker.MarkUnhealthy(c.Backend.Name, c.Model, fmt.Sprintf("转发失败: %v", err))
			continue
		}

		log.Printf("[代理] ✓ 后端 %s/%s 成功响应", c.Backend.Name, c.Model)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	log.Printf("[代理] ✗ 所有候选后端均失败")
	writeError(w, http.StatusBadGateway, "all_backends_failed",
		fmt.Sprintf("所有候选后端均请求失败，最后错误: %v", lastErr))
}

// handleStream 流式请求，流建立前支持 fallback
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, req openai.ChatCompletionRequest, candidates []healthcheck.BackendWithModel) {
	var lastErr error

	for i, c := range candidates {
		req.Model = c.Model
		client := h.getClient(c.Backend.Name)
		ctx, cancel := context.WithCancel(r.Context())

		log.Printf("[代理] [流式] [%s] 尝试后端 %s/%s (%d/%d)", h.cfg.Strategy, c.Backend.Name, c.Model, i+1, len(candidates))

		stream, err := client.CreateChatCompletionStream(ctx, req)
		if err != nil {
			cancel()
			lastErr = err
			log.Printf("[代理] [流式] 后端 %s/%s 创建流失败: %v", c.Backend.Name, c.Model, err)
			h.checker.MarkUnhealthy(c.Backend.Name, c.Model, fmt.Sprintf("创建流失败: %v", err))
			continue
		}

		log.Printf("[代理] [流式] ✓ 后端 %s/%s 流已建立", c.Backend.Name, c.Model)
		h.streamResponse(w, stream)
		stream.Close()
		cancel()
		return
	}

	log.Printf("[代理] [流式] ✗ 所有候选后端均失败")
	writeError(w, http.StatusBadGateway, "all_backends_failed",
		fmt.Sprintf("所有候选后端均请求失败，最后错误: %v", lastErr))
}

// streamResponse 将 openai 流式响应转发给客户端（标准 SSE 格式）
func (h *Handler) streamResponse(w http.ResponseWriter, stream *openai.ChatCompletionStream) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Println("[代理] [流式] ResponseWriter 不支持 Flush")
		return
	}

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		if err != nil {
			log.Printf("[代理] [流式] 读取流错误: %v", err)
			errData, _ := json.Marshal(errorResponse{
				Error: errorDetail{
					Message: fmt.Sprintf("流读取错误: %v", err),
					Type:    "stream_error",
					Code:    "500",
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", errData)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}

		data, err := json.Marshal(resp)
		if err != nil {
			log.Printf("[代理] [流式] 序列化响应失败: %v", err)
			continue
		}

		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
}

// Embeddings 处理 /v1/embeddings 请求，支持自动 fallback
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 方法")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	var req openai.EmbeddingRequestStrings
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "解析请求体失败: "+err.Error())
		return
	}

	// 获取候选后端列表
	var candidates []healthcheck.BackendWithModel
	reqModel := string(req.Model)

	if reqModel != "" {
		candidates = h.checker.BackendsForModel(reqModel, "embedding")
		if len(candidates) == 0 {
			if h.cfg.Fallback == "failed" {
				h.handleNoBackend(w, reqModel)
				return
			}
			candidates = h.checker.AllHealthyBackendModels("embedding")
			if len(candidates) == 0 {
				writeError(w, http.StatusServiceUnavailable, "no_model_available",
					fmt.Sprintf("Embedding 模型 %q 不可用，且没有任何其他可用的 embedding 后端", reqModel))
				return
			}
			log.Printf("[代理] [embedding] [failover] 模型 %q 不可用，自动切换", reqModel)
		}
	} else {
		candidates = h.checker.AllHealthyBackendModels("embedding")
		if len(candidates) == 0 {
			writeError(w, http.StatusServiceUnavailable, "no_model_available",
				"当前没有任何可用的 embedding 后端模型")
			return
		}
	}

	candidates = h.reorder(candidates)
	h.handleEmbedding(w, req, candidates)
}

// handleEmbedding 逐个尝试候选后端，失败自动切换
func (h *Handler) handleEmbedding(w http.ResponseWriter, req openai.EmbeddingRequestStrings, candidates []healthcheck.BackendWithModel) {
	var lastErr error

	for i, c := range candidates {
		req.Model = openai.EmbeddingModel(c.Model)
		client := h.getClient(c.Backend.Name)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)

		log.Printf("[代理] [embedding] [%s] 尝试后端 %s/%s (%d/%d)", h.cfg.Strategy, c.Backend.Name, c.Model, i+1, len(candidates))

		resp, err := client.CreateEmbeddings(ctx, req)
		cancel()

		if err != nil {
			lastErr = err
			log.Printf("[代理] [embedding] 后端 %s/%s 失败: %v", c.Backend.Name, c.Model, err)
			h.checker.MarkUnhealthy(c.Backend.Name, c.Model, fmt.Sprintf("embedding 转发失败: %v", err))
			continue
		}

		log.Printf("[代理] [embedding] ✓ 后端 %s/%s 成功响应", c.Backend.Name, c.Model)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	log.Printf("[代理] [embedding] ✗ 所有候选后端均失败")
	writeError(w, http.StatusBadGateway, "all_backends_failed",
		fmt.Sprintf("所有 embedding 候选后端均请求失败，最后错误: %v", lastErr))
}

// handleNoBackend 处理指定模型没有可用后端的情况
func (h *Handler) handleNoBackend(w http.ResponseWriter, model string) {
	found := false
	for _, b := range h.cfg.Backends {
		for _, m := range b.EffectiveModels() {
			if m == model {
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	if found {
		writeError(w, http.StatusServiceUnavailable, "model_unavailable",
			fmt.Sprintf("模型 %q 当前不可用，所有后端均未通过健康检查", model))
		return
	}

	if !h.checker.HasAnyHealthy() {
		writeError(w, http.StatusServiceUnavailable, "no_model_available",
			"当前没有任何可用的后端模型")
		return
	}

	healthyModels := h.checker.AllHealthyModels()
	writeError(w, http.StatusNotFound, "model_not_found",
		fmt.Sprintf("未找到模型 %q 的配置，可用模型: %s", model, strings.Join(healthyModels, ", ")))
}

// ListModels 返回所有健康的模型
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET 方法")
		return
	}

	healthyModels := h.checker.AllHealthyModels()
	models := make([]modelInfo, 0, len(healthyModels))
	for _, m := range healthyModels {
		models = append(models, modelInfo{
			ID:      m,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: "ai-proxy",
		})
	}

	writeJSON(w, http.StatusOK, modelsResponse{
		Object: "list",
		Data:   models,
	})
}

// Status 返回所有后端状态
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET 方法")
		return
	}

	statuses := h.checker.AllStatuses()
	healthyCount := 0
	for _, s := range statuses {
		if s.Healthy {
			healthyCount++
		}
	}

	overall := "healthy"
	if healthyCount == 0 {
		overall = "unhealthy"
	} else if healthyCount < len(statuses) {
		overall = "degraded"
	}

	writeJSON(w, http.StatusOK, statusResponse{
		Status:   overall,
		Strategy: h.cfg.Strategy,
		Fallback: h.cfg.Fallback,
		Healthy:  healthyCount,
		Total:    len(statuses),
		Backends: statuses,
	})
}

// Health 简单存活检查
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if h.checker.HasAnyHealthy() {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"healthy_models": h.checker.AllHealthyModels(),
		})
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"reason": "没有可用的后端模型",
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, errorResponse{
		Error: errorDetail{
			Message: message,
			Type:    errType,
			Code:    fmt.Sprintf("%d", status),
		},
	})
}
