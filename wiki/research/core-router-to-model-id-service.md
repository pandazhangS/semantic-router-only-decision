# 语义路由核心模块裁剪评估：输入上下文 → 输出 model id 的决策服务

> 调研日期：2026-08-27
> 目标：把 vLLM Semantic Router 的核心决策链路裁剪成一个独立服务——接收请求上下文（prompt/messages），输出决策结果（model id 或 model list），供 Higress 网关消费后转发。不需要 Envoy ExtProc 集成、请求转发、响应处理、插件执行。

## 1. 结论摘要

- **核心决策链路已经存在且与 ExtProc 解耦**：`ClassificationService.ClassifyIntent`（`src/semantic-router/pkg/services/classification.go:164`）已经实现"输入文本 → 信号评估 → 决策匹配 → 输出 RecommendedModel"，并通过 `POST /api/v1/classify/intent` HTTP API 暴露。
- **裁剪工作量比预期小**：不需要重写路由逻辑，主要工作是换一个 main 入口（去掉 ExtProc server / K8s controller / 模型下载等外围），并新增/调整一个"决策"端点返回 model id + model list。
- **最小方案约 2-4 人日**（复用现有 apiserver + ClassificationService）；深度裁剪独立 module 约 1-2 周。
- **最大不确定项是 embedding 信号评估的模型依赖**：本地推理（candle-binding，需下载模型）还是远程 HTTP classifier，直接决定部署复杂度和启动流程。

## 2. 现有核心决策链路（请求 → model id）

### 2.1 ExtProc 模式下的完整链路

```
handleRequestBody                          pkg/extproc/processor_req_body.go:32
└─ runRequestPreRoutingStages              pkg/extproc/processor_req_body_prepare.go:101
   ├─ resolveEntrypointForRequest          入口(model 别名) → recipe
   ├─ performDecisionEvaluation            pkg/extproc/req_filter_classification.go:22  ★核心
   │  ├─ prepareSignalEvaluationInput      组装信号评估输入(文本/会话事实)
   │  ├─ evaluateSignalsForDecision        pkg/extproc/req_filter_classification_runtime.go:31
   │  │  └─ classifier.EvaluateAllSignalsWithHeaders   pkg/classification/  ★信号评估
   │  ├─ runDecisionEngine                 req_filter_classification_runtime.go:154
   │  │  └─ classifier.EvaluateDecisionWithEngine → decision.DecisionEngine  pkg/decision/ ★决策匹配
   │  └─ finalizeDecisionEvaluation        req_filter_classification_runtime.go:216
   │     └─ selectDecisionRuntimeModel → selection.Selector  pkg/selection/ ★模型选择
   ├─ handleFastResponse / applyRateLimitAndCacheChecks / executeRAGPlugin   ← 可裁剪
└─ handleModelRouting                      pkg/extproc/processor_req_body.go:138  ← 转发/改写，裁剪
   └─ handleAutoRouting → handleAutoModelRouting   ← 只保留"选中模型"结果，去掉 upstream 调用
```

**关键结论**：`performDecisionEvaluation` 返回 `(decisionName, confidence, reasoningDecision, selectedModel)`，selectedModel 就是目标 model id。它不依赖 ExtProc 协议，只依赖 `RequestContext`（可替换为轻量输入结构）。

### 2.2 已存在的独立 API（无需 ExtProc）

`ClassificationService.ClassifyIntent`（`pkg/services/classification.go:164`）：

```
输入 IntentRequest{ text / messages / model / options }
  → classifier.EvaluateAllSignalsWithRequestFacts(...)   信号评估
  → classifier.EvaluateDecisionWithEngine(signals)       决策引擎匹配
  → resolveIntentCategory / buildIntentResponseFromSignals
输出 IntentResponse{ Classification{category, confidence},
                     RecommendedModel,      ← model id 已输出
                     RoutingDecision }
```

已通过 `POST /api/v1/classify/intent` 暴露（`pkg/apiserver/`，见 `website/docs/api/apiserver.md`）。另有 `/api/v1/eval`（评估所有信号）、`/api/v1/classify/combined` 等。

**缺口**：现有响应只给单个 RecommendedModel，没有候选 model list；输入是 `text` 字段而非完整 messages 数组。需要新增一个决策端点（约 100-200 行 handler + 响应结构）。

## 3. 模块裁剪分类

按"输入 → model id"主链路依赖关系分类（代码量统计自 2026-08-27）：

### 3.1 核心必需（决策链路，约 8-10 个包）

| 包 | 规模 | 职责 |
| --- | --- | --- |
| `pkg/config/` | ~30 文件 | 配置/recipe 解析、校验、热加载（`RouterConfig`、`Decision`、`ProjectionConfig`） |
| `pkg/classification/` | ~30 文件 | 信号评估编排（classifier.go 为 hotspot）、embedding/keyword/domain 等信号族 |
| `pkg/decision/` | 12 文件 / 2.5k 行 | 决策引擎 `DecisionEngine.EvaluateDecisionsWithSignals` |
| `pkg/selection/` | 58 文件 / 19.5k 行 | 模型选择算法（Selector 接口：KNN/Elo/AutoMix/Hybrid/MultiFactor/Prompt/RLDriven 等） |
| `pkg/modelselection/` | 6 文件 / 6.6k 行 | 模型选择注册表（另一个 Selector 体系） |
| `pkg/services/` | 24 文件 / 4.5k 行 | ClassificationService（API 服务层） |
| `pkg/embedding/` | 3 文件 / 0.7k 行 | embedding 推理接口 |
| `pkg/ir/` | 5 文件 / 5.5k 行 | 中间表示（信号/投影执行） |
| `pkg/observability/` | — | 日志/指标/tracing |
| `pkg/consts/`、`pkg/utils/` | — | 基础工具 |
| `pkg/routerruntime/` | 6 文件 / 0.9k 行 | 运行时注册表（组件组装） |
| `pkg/startupstatus/` | 3 文件 / 0.3k 行 | 启动状态（可简化） |
| `candle-binding/`、`ml-binding/`、`nlp-binding/` | Rust | 本地 embedding/分类推理（若用本地模型） |

### 3.2 可选（按需保留）

| 包 | 规模 | 说明 |
| --- | --- | --- |
| `pkg/sessiontelemetry/` | 13 文件 / 2.3k 行 | reask 信号、会话状态需要；单轮决策可裁剪 |
| `pkg/promptcompression/` | 10 文件 / 3.9k 行 | 信号评估前的文本压缩（长上下文时有用） |
| `pkg/latency/` | 3 文件 / 0.8k 行 | latency-aware 选择算法需要 |
| `pkg/ratelimit/` | 5 文件 / 1.1k 行 | 限流，可选 |
| `pkg/authz/` | 5 文件 / 1.0k 行 | 鉴权，可选 |
| `pkg/inflight/` | 2 文件 / 0.3k 行 | 在途请求计数，很小可留 |
| `pkg/modeldownload/` | 10 文件 / 2.3k 行 | 本地模型下载；远程 embedding 时裁剪 |
| `pkg/apiserver/` | 99 文件 / 18.4k 行 | 只需 classification 相关路由（约 1/4），其余裁剪 |

### 3.3 外围可裁剪（与决策无关）

| 包 | 规模 | 说明 |
| --- | --- | --- |
| `pkg/extproc/` | ~40 文件 | **Envoy ExtProc 服务器**（主裁剪对象，含转发/改写/流式） |
| `pkg/looper/` | 67 文件 / 18.8k 行 | 多模型执行（cascade/panel/workflow）；只输出单 model id 则裁剪 |
| `pkg/vectorstore/` | 50 文件 / 11.6k 行 | 向量存储（RAG） |
| `pkg/memory/` | 41 文件 / 10.8k 行 | 长期记忆 |
| `pkg/anthropic/` | 15 文件 / 7.7k 行 | Anthropic 协议转换（只做决策不需要） |
| `pkg/openai/` | 5 文件 / 1.2k 行 | OpenAI 协议（决策服务只需解析 messages） |
| `pkg/tools/`、`pkg/mcp/` | 21 文件 / 4k 行 | 工具调用 |
| `pkg/imagegen/`、`pkg/nlgen/` | — | 图像/自然语言生成 |
| `pkg/responseapi/`、`pkg/responsestore/` | 15 文件 / 4.2k 行 | 响应 API/存储 |
| `pkg/routerreplay/` | 6 文件 / 1.7k 行 | 回放 |
| `pkg/cache/` | — | 响应缓存 |
| `pkg/k8s/` | 9 文件 / 2.4k 行 | K8s 控制器 |
| `pkg/milvus/`、`pkg/hnsw/`、`pkg/postgres/` | — | 存储后端 |
| `pkg/modelpricing/`、`pkg/publicmodels/`、`pkg/modelinventory/` | — | 模型清单/定价 |
| `pkg/dsl/`、`pkg/wasm/`、`pkg/fusioneval/` | — | DSL 解析（若只用 config.yaml 可裁剪）、实验性 |
| `pkg/contextcompression/` | 14 文件 / 3k 行 | 上下文压缩插件 |
| `pkg/logo/` | — | 启动 logo |

## 4. 裁剪方案与工作量

### 方案 A：复用现有 apiserver + ClassificationService（推荐，最小改动）

**做法**：
1. 新写一个 main 入口（`cmd/decision-server/main.go`，约 200 行）：加载 config → 初始化 classifier（embedding）→ 启动 apiserver 的 classification 路由子集。
2. 在 `pkg/services/` 新增一个 `DecideModel` 方法（约 100 行）：输入 messages → 复用 `EvaluateAllSignalsWithRequestFacts` + `EvaluateDecisionWithEngine` + `selectDecisionRuntimeModel`，输出 `{model_id, model_list, decision_name, confidence}`。
3. 在 apiserver 注册 `POST /v1/decide`（约 100 行 handler）。
4. 去掉 main 里的：ExtProc server、K8s controller、modeldownload（若远程 embedding）、metrics server（可选保留）。

**改动面**：新增 1 个 main + 1 个 handler + 1 个 service 方法；**不删**现有代码（extproc 等保留但不在新 main 里启动）。
**工作量**：**2-4 人日**（含测试）。风险最低，后续可逐步裁剪。

### 方案 B：深度裁剪独立 module

**做法**：从 `src/semantic-router/` 复制核心包（config/classification/decision/selection/modelselection/services/embedding/ir/observability 等）到新 module，删除外围包，重写 main。
**难点**：包间依赖梳理（selection 依赖 latency/sessiontelemetry，classification 依赖 promptcompression 等），需要逐个 `go build` 迭代；`go.mod` 依赖从 40+ 减到 ~15 个。
**工作量**：**1-2 周**（含依赖清理和测试迁移）。收益是二进制体积和启动时间显著下降。

### 方案 C：从零实现轻量版

**做法**：参考 recipe 结构（config.yaml + recipe.dsl），用 Go 重新实现 keyword/embedding 信号 + 决策匹配 + 模型选择。
**工作量**：**2-3 周**。不推荐——现有代码已解耦，重写会丢失 recipe.dsl、投影、多算法选择等成熟能力。

## 5. 关键决策点（需确认）

1. **embedding 信号评估的模型来源**：
   - 本地推理（candle-binding，需下载模型，启动慢但无外部依赖）
   - 远程 HTTP classifier（`pkg/classification/http_classifier.go` 已支持，部署简单）
   - 这决定是否需要 `modeldownload` 和 Rust binding。
2. **输出契约**：只要 `model_id`，还是 `model_list`（候选 + 置信度）？Higress 侧如何消费（header 注入 / 响应体）？
3. **输入格式**：OpenAI messages 数组（推荐，与 Higress 透传一致）还是纯 text？
4. **是否保留 recipe.dsl**：只用 config.yaml 可裁剪 `pkg/dsl/`；保留则支持现有 recipe 资产。
5. **部署形态**：独立二进制 + 配置文件，还是容器镜像？是否需要健康检查/就绪探针（`/health`、`/ready` 已有实现）。

## 6. 参考文件索引

- 核心链路：`src/semantic-router/pkg/extproc/processor_req_body.go`、`processor_req_body_prepare.go`、`req_filter_classification.go`、`req_filter_classification_runtime.go`
- 已有 API：`src/semantic-router/pkg/services/classification.go`、`classification_recommendation.go`、`src/semantic-router/pkg/apiserver/`
- 配置模型：`src/semantic-router/pkg/config/config.go`、`decision_config.go`、`projection_config.go`
- 架构文档：`website/docs/overview/semantic-router-overview.md`、`signal-driven-decisions.md`、`website/docs/api/apiserver.md`
- 示例 recipe：`config/recipes/balance/config.yaml`、`recipe.dsl`