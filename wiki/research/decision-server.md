# decision-server：语义路由决策服务

> 裁剪自 vLLM Semantic Router（`decision-only` 分支），只保留核心决策链路：
> **输入请求上下文 → 输出 model_id + model_list**。无 Envoy ExtProc、无请求转发、无插件执行。

## 1. 服务定位

- 独立 HTTP 服务，供 Higress 等网关调用获取路由决策，再按 `model_id` 转发到后端模型服务
- 无状态：不保存会话、不缓存响应、不执行多模型编排
- 核心链路：`config 解析 → 信号评估（keyword/embedding 等）→ 决策匹配 → 模型选择`

## 2. 构建与启动

```bash
# Ubuntu（需要 Go 1.25+、gcc、Rust 工具链编译 candle-binding/nlp-binding）
cd src/semantic-router
go build -o decision-server ./cmd/decision-server

# 启动（配置示例见 config/decision-server.yaml）
EMBEDDING_API_KEY=xxx ./decision-server -config config/decision-server.yaml -listen :8080
```

参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-config` | `config.yaml` | 路由配置文件路径 |
| `-listen` | `:8080` | HTTP 监听地址 |

## 3. API

### POST /v1/decide — 核心决策端点

输入请求上下文（OpenAI messages 或纯文本），输出选中的 model id 和候选模型列表。

**请求体**（字段与 `/api/v1/classify/intent` 兼容）：

```json
{
  "model": "vllm-sr/auto",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant."},
    {"role": "user", "content": "Debug this Python stack trace and explain the likely bug."}
  ],
  "metadata": {"user_id": "u-123"}
}
```

或纯文本：

```json
{
  "text": "Debug this Python stack trace and explain the likely bug."
}
```

**响应**：

```json
{
  "model_id": "qwen/qwen3.5-rocm",
  "model_list": [
    {"model": "qwen/qwen3.5-rocm", "reasoning_effort": "medium", "use_reasoning": true}
  ],
  "decision_name": "code_general",
  "confidence": 0.87,
  "matched_rules": ["code_request_markers", "code_general"],
  "category": "code_general",
  "recipe": "default",
  "selection_status": "selected",
  "selection_method": "static",
  "selection_reason": "single declared candidate",
  "processing_time_ms": 12
}
```

字段说明：

| 字段 | 说明 |
| --- | --- |
| `model_id` | 选中的模型 id（Higress 用它做转发目标） |
| `model_list` | 匹配决策的全部候选模型（含 reasoning_effort/use_reasoning，可做降级） |
| `decision_name` | 命中的决策名；无匹配时为 `default` |
| `confidence` | 决策置信度 |
| `matched_rules` | 命中的信号规则 |
| `category` | 分类结果（决策名或分类器输出） |
| `recipe` | 命中的 recipe 名 |
| `selection_status` | `selected`（已选）/ `execution_required`（looper 算法，需执行后确定） |
| `selection_method` | 选择算法（static/elo/hybrid/multi_factor 等） |
| `selection_reason` | 选择原因 |

**错误响应**：

```json
{"error": "text cannot be empty"}
```

| 状态码 | 场景 |
| --- | --- |
| 400 | 请求体非法 / 空文本 |
| 500 | 分类器不可用 / 决策评估失败 |

### GET /health — 存活探针

```json
{"status": "ok"}
```

### GET /ready — 就绪探针

```json
{"status": "ready"}
```

## 4. 配置说明

示例：`config/decision-server.yaml`。关键配置块：

| 配置块 | 说明 |
| --- | --- |
| `providers.models` | 候选模型池（name/provider_model_id/backend_refs/pricing） |
| `providers.defaults.default_model` | 无决策匹配时的兜底模型 |
| `routing.signals` | 信号定义：`keywords`（用 `method: regex` 纯 Go 匹配）、`embeddings`（语义相似度，本地或远程） |
| `routing.decisions` | 决策规则：conditions（信号组合）+ modelRefs（候选模型）+ priority |
| `global.model_catalog.embeddings.semantic` | embedding 配置：`backend: openai_compatible` + `endpoint`（base_url/model/api_key_env/dimensions），或 candle 后端 + 本地模型路径 |

注意（2026-09-01 k3s 实测修正）：

- **嵌入配置必须写在 `global.model_catalog.embeddings.semantic`**。写在顶层 `model_catalog` 会被解析器静默忽略（backend 回落 candle → 启动时因找不到本地 mmBERT 模型崩溃，报 `FileNotFound("models/mmbert-embed-32k-2d-matryoshka/config.json")` 即此坑）
- `endpoint.dimensions` 与 `embedding_config.target_dimension` 必须一致（代码硬校验）
- keyword 信号用 `method: regex`（纯 Go）；`bm25/ngram` 方法需要 nlp-binding（Rust），本服务保留支持但需 Ubuntu 编译
- ~~embedding 信号评估走本地 mmBERT 模型，远程 embedding 只用于 selection 算法~~ **已实测推翻**：`backend: openai_compatible` 时 embedding 信号直接走远程 provider（`embeddingProviderForRules` 装配），且启动跳过本地模型加载；远程嵌入同时驱动信号评估与 selection query 向量。早先结论源于上面的配置位置错误
- 内置（candle）模式：`qwen3_model_path` 指向 Qwen3-Embedding-0.6B；`mmbert_model_path` 必须显式置空（canonical 默认路径在本机无文件会启动失败）
- `enable_soft_matching` 规范默认开启（软匹配门槛 `min_score_threshold`=0.5），真实模型短文本基线相似度高，会放进无关请求；生产建议显式关闭，按每条信号 threshold 硬门限校准
- domain/PII/jailbreak 等分类器信号需要本地 mmBERT 模型（candle-binding），远程模式下不配置对应模型即可跳过
- 已知限制：selection 侧 qwen3 向量走 `GetEmbeddingBatched` 但启动未调用 batched 初始化（日志有 `batched embedding model not initialized`）；静态选择不受影响，用 router_dc/hybrid 前需修复

## 5. Higress 集成示例

Higress 侧用 WASM 插件或自定义 filter 调用决策服务，按 `model_id` 改写上游请求：

```text
客户端请求 → Higress → POST /v1/decide {messages} → 得到 model_id
                    → 改写请求体 model 字段为 model_id → 转发到对应后端
```

## 6. Ubuntu 验证步骤

已在 Ubuntu 24.04 测试机验证通过（2026-08-27）：

```bash
# 1. 安装依赖（国内镜像）
#    go: https://golang.google.cn/dl/go1.25.0.linux-amd64.tar.gz → ~/go-sdk
#    rust: RUSTUP_DIST_SERVER=https://rsproxy.cn rustup-init.sh -y
#    cargo 镜像: ~/.cargo/config.toml → sparse+https://rsproxy.cn/index/
#    GOPROXY=https://goproxy.cn,direct
# 2. 编译 Rust bindings（candle 需 --no-default-features 禁用 CUDA）
cd candle-binding && cargo build --release --no-default-features
cd ../nlp-binding && cargo build --release
# 3. 编译服务
cd ../src/semantic-router && go build ./...
# 4. 启动（需 LD_LIBRARY_PATH 指向 .so）
LD_LIBRARY_PATH=.../candle-binding/target/release:.../nlp-binding/target/release \
  ./decision-server -config config/decision-server.yaml -listen :8080
# 5. 冒烟测试
curl -s localhost:8080/health
curl -s -X POST localhost:8080/v1/decide -d '{"text":"Debug this Python stack trace"}'
```

实测结果（keyword 信号）：

| 输入 | 输出 |
| --- | --- |
| "Debug this Python stack trace..." | `code_general` → `qwen/qwen3.5-rocm` |
| "Prove the theorem rigorously from first principles" | `reasoning_deep` → `google/gemini-3.1-pro`（model_list 2 候选） |
| "hello world" | `default` 兜底 → `qwen/qwen3.5-rocm` |

## 7. 裁剪范围

**保留**（20 个包）：`config`、`classification`、`decision`、`selection`、`services`、`embedding`、`consts`、`utils`、`observability`、`projectiontrace`、`inflight`、`latency`、`headers`、`internalauth`、`modelruntime`、`ir`、`promptcompression`、`startupstatus`、`selection/lookuptable`、`cmd/decision-server`

**删除**（33 个包/入口）：`extproc`（Envoy ExtProc 服务器）、`apiserver`/`apis`（管理 API）、`vectorstore`/`memory`/`milvus`/`hnsw`/`postgres`（存储）、`looper`（多模型执行）、`k8s`（控制器）、`anthropic`/`openai`（协议转换）、`tools`/`mcp`（工具）、`imagegen`/`nlgen`/`responseapi`/`responsestore`（生成/响应）、`routerreplay`/`cache`（回放/缓存）、`dsl`/`wasm`/`fusioneval`（DSL/实验）、`modelpricing`/`publicmodels`/`modelinventory`/`modeldownload`/`logo`、`contextcompression`/`ratelimit`/`authz`、`modelselection`（ML 选择器，依赖 ml-binding）、`sessiontelemetry`/`routerruntime`、原 `cmd/main.go` 等

**保留的 cgo 依赖**：`candle-binding`（本地推理，配置远程时运行时不加载）、`nlp-binding`（keyword 的 bm25/ngram 方法，默认 regex 不触发）

## 8. k3s 部署与指标实测（2026-09-01）

在 k3s 集群（control-plane 16C/62G，Ubuntu 24.04）以独立 namespace 部署 decision-server + mock 嵌入服务：

- 部署形态：ConfigMap 挂载 3 份配置（keyword / 远程嵌入 / 内置嵌入），切 `-config` 参数即切模式；镜像本机构建后经特权 helper pod 导入 k3s containerd（`imagePullPolicy: Never` + nodeSelector 固定）
- 外部嵌入 mock：OpenAI 兼容 `/v1/embeddings`，确定性词元哈希向量（1024 维，共享词元 → 高相似度），支持 `-latency` 模拟真实后端延迟
- 内置嵌入：Qwen/Qwen3-Embedding-0.6B（hf-mirror 下载约 1.2GB），candle CPU 推理，hostPath 挂载

三种配置均通过功能冒烟（code/reasoning/simple 正例 + hello world 兜底 + messages 格式）。

### 指标（loadgen c=8 / 60s / NodePort，零错误）

| 指标 | keyword 基线 | 远程嵌入 mock 0ms | mock 70ms（典型） | mock 100ms | 内置 Qwen3-0.6B |
| --- | --- | --- | --- | --- | --- |
| ready 时间 | ~1.7s | <1s | <1s | <1s | ~5s（含模型加载） |
| 内存 RSS | 14Mi | 21Mi | ~21Mi | ~21Mi | ~2.5GB |
| RPS | 14,573 | 1,681 | 111.2 | 78.4 | 15.5 |
| 延迟 p50 | 0.50ms | 3.09ms | 71.8ms | 101.8ms | 509ms |
| 延迟 p99 | 1.51ms | 19.5ms | 74.2ms | 104.4ms | 718ms |

结论：

- 远程嵌入模式下吞吐 ≈ `并发 / 嵌入延迟`（实测与公式吻合），端到端延迟由嵌入服务主导；决策服务自身开销 ~1-2ms
- 内置模式单请求 CPU 推理 100~230ms，并发下线程争用放大延迟；内存为主要代价（~2.5GB）
- keyword 与 embedding 混布可显著降低嵌入调用比例：确定词先短路

### 运维产物

- 内置 API 指南随镜像分发：`GET /docs`（源文件 `src/semantic-router/cmd/decision-server/api_guide.md`，wiki 同步副本 `wiki/guides/decision-server-api.md`）
- 离线构建包：基础镜像（rust/golang/debian）+ 构建产物镜像存档于测试机用户目录 `decision-server-offline/`，避免内网无法拉取基础镜像导致构建失败