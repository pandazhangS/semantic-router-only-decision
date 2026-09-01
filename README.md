# decision-server

Standalone semantic routing **decision service**, trimmed from
[vLLM Semantic Router](https://github.com/vllm-project/semantic-router) (`decision-only` branch).

语义路由决策服务:输入请求上下文(OpenAI messages 或纯文本)→ 输出选中的 `model_id` 与候选模型列表。
不含 Envoy ExtProc、请求转发、插件执行等外围链路,供 Higress 等网关在转发前调用获取路由决策。

## API

| 端点 | 说明 |
| --- | --- |
| `POST /v1/decide` | 核心决策端点:输入上下文 → `model_id` + `model_list`(含置信度) |
| `GET /health` | 存活探针 |
| `GET /ready` | 就绪探针(路由组件构建完成后才开放监听) |
| `GET /docs` | 内置运行手册:路由 API、外部嵌入配置、阈值调优指南(markdown) |

```bash
curl -s -X POST localhost:8080/v1/decide \
  -d '{"text":"Debug this Python stack trace and explain the likely bug"}'
```

## Build

需要 Go 1.25+、gcc、Rust(candle-binding / nlp-binding,`--no-default-features` 禁用 CUDA):

```bash
(cd candle-binding && cargo build --release --no-default-features)
(cd nlp-binding && cargo build --release)
cd src/semantic-router && go build -o decision-server ./cmd/decision-server
```

或直接用 Docker 多阶段构建(离线环境先导入基础镜像包):

```bash
docker build -t decision-server:latest .
```

## Run

```bash
EMBEDDING_API_KEY=xxx ./decision-server -config config/decision-server.yaml -listen :8080
```

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-config` | `config.yaml` | 路由配置文件路径 |
| `-listen` | `:8080` | HTTP 监听地址 |

## Configuration

- 示例配置:`config/decision-server.yaml`(keyword 信号,零模型依赖即可启动)
- 嵌入模型配置必须位于 **`global.model_catalog.embeddings.semantic`**(顶层 `model_catalog` 会被解析器忽略)
- 外部嵌入:`backend: openai_compatible` + `endpoint`(OpenAI 兼容 `/v1/embeddings`)
- 内置嵌入:`backend: candle` + `qwen3_model_path`(如 Qwen3-Embedding-0.6B)
- 阈值调优、匹配逻辑与实测数据:运行 `curl /docs` 或阅读 [wiki/guides/decision-server-api.md](wiki/guides/decision-server-api.md)

## Repository layout

```
candle-binding/        Rust 本地推理库(cgo,embedding/classifier)
nlp-binding/           Rust NLP 库(keyword 信号 bm25/ngram 方法)
src/semantic-router/   Go 决策服务(cmd/decision-server + pkg/*)
config/                配置示例
wiki/                  调研与运维文档
```

## License

Apache-2.0
