APP_NAME  := ai-proxy
IMAGE     := $(APP_NAME)
TAG       := latest

.PHONY: build run clean docker docker-run

# 本地编译
build:
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(APP_NAME) .

# 本地运行
run: build
	./$(APP_NAME) -config config.yaml

# 清理
clean:
	rm -f $(APP_NAME)

# 构建 Docker 镜像
docker:
	docker build -t $(IMAGE):$(TAG) .

# Docker 运行（挂载配置文件）
docker-run: 
	docker run --rm -p 8081:8081 \
		-v $(PWD)/current.yaml:/etc/ai-proxy/config.yaml:ro \
		$(IMAGE):$(TAG)
