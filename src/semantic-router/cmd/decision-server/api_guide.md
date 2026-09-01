# decision-server API 与配置指南

> 随镜像分发的运行手册:路由 API、外部嵌入配置、阈值调优。
> 服务内置本文副本,运行中可执行 `curl http://<service>/docs` 直接获取。
> 与 `wiki/guides/decision-server-api.md` 保持同步。

decision-server 是语义路由决策服务(裁剪自 vLLM Semantic Router,`decision-only` 分支):
**输入请求上下文(OpenAI messages 或纯文本)→ 输出选中的 model_id 与候选模型列表**。
不含 Envoy ExtProc、请求转发、插件执行等外围链路,供 Higress 等网关在转发前调用获取路由决策。

---

## 1. API 参考

### POST /v1/decide — 核心决策端点

**请求**(两种形式,二选一):

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

或纯文本:

```json
{"text": "Debug this Python stack trace and explain the likely bug."}
```

**响应**:

```json
{
  "model_id": "qwen/qwen3.5-rocm",
  "model_list": [
    {"model": "qwen/qwen3.5-rocm", "reasoning_effort": "medium", "use_reasoning": true}
  ],
  "decision_name": "code_general",
  "confidence": 0.9365,
  "matched_rules": ["embedding:code_request_embedding"],
  "category": "code_general",
  "recipe": "default",
  "selection_status": "selected",
  "selection_method": "static",
  "selection_reason": "single declared candidate",
  "processing_time_ms": 173
}
```

| 字段 | 说明 |
| --- | --- |
| `model_id` | 选中的模型 id,网关用它改写转发目标 |
| `model_list` | 命中决策的全部候选模型(含 reasoning_effort/use_reasoning,可做降级) |
| `decision_name` | 命中的决策名;无匹配时为 `default` |
| `confidence` | 决策置信度(来源:keyword 命中=1;embedding 命中=相似度分数) |
| `matched_rules` | 命中的信号规则,`keyword:<name>` / `embedding:<name>` 前缀区分类型 |
| `category` | 分类结果(决策名或分类器输出;无匹配为 `other`) |
| `recipe` | 命中的 recipe 名 |
| `selection_status` | `selected`(已选定)/ `execution_required`(looper 类算法,需执行后确定) |
| `selection_method` | 模型选择算法(static/elo/router_dc/hybrid/multi_factor 等) |
| `selection_reason` | 选择原因 |
| `processing_time_ms` | 决策耗时 |

**错误**:`400` 请求体非法/空文本(`{"error": "..."}`);`500` 决策评估失败(如远程嵌入不可达)。

### 其他端点

| 端点 | 说明 |
| --- | --- |
| `GET /health` | 存活探针,`{"status":"ok"}` |
| `GET /ready` | 就绪探针,`{"status":"ready"}`;HTTP 监听在路由组件构建完成后才开启,Ready 即代表决策链路可用 |
| `GET /docs` | 返回本指南(markdown) |

---

## 2. 路由配置

完整示例见仓库 `config/decision-server.yaml`。配置分三层:

```
providers  → 候选模型池(name/provider_model_id/backend_refs/pricing/reliability)
routing    → 信号(signals)+ 决策(decisions)+ modelCards
global     → 路由器全局行为 + 模型资产(model_catalog)
```

### 2.1 决策链路

```
请求文本 ──► 信号评估(keyword regex / embedding 相似度)──► 决策匹配(conditions,按 priority)
      ──► 模型选择(modelRefs 候选 + 选择算法)──► model_id + model_list
无决策命中 ──► providers.defaults.default_model 兜底
```

### 2.2 信号(signals)

**keyword 信号**(纯 Go regex,零模型依赖):

```yaml
routing:
  signals:
    keywords:
      - name: code_request_markers
        operator: OR
        method: regex          # bm25/ngram 需 nlp-binding(.so),regex 无依赖
        keywords: ["code", "stack trace", "debug", ...]
        case_sensitive: false
```

**embedding 信号**(语义相似度,支持本地或远程嵌入,见第 3 节):

```yaml
routing:
  signals:
    embeddings:
      - name: code_request_embedding
        threshold: 0.6         # 相似度 >= threshold 即命中
        candidates:            # 候选文案 = 该信号的语义原型,启动时预计算向量
          - "Debug this Python stack trace and explain the likely bug"
          - "My program crashes with an error, help me fix it"
```

### 2.3 决策(decisions)

```yaml
routing:
  decisions:
    - name: code_general
      priority: 100            # 数值大者优先
      rules:
        operator: OR           # 多 condition 间的组合逻辑
        conditions:
          - name: code_request_embedding
            type: embedding    # keyword | embedding | ...
      modelRefs:               # 命中后的候选模型(首个为默认选择)
        - model: qwen/qwen3.5-rocm
          reasoning_effort: medium
          use_reasoning: true
```

**契约要求**(v0.3 规范):`providers.defaults.default_model` 必须同时登记在 `routing.modelCards` 中,否则启动失败。

---

## 3. 外部嵌入配置(openai_compatible)

### 3.1 配置位置(易错点!)

v0.3 规范中嵌入模型配置位于 **`global.model_catalog.embeddings.semantic`**。
写在顶层 `model_catalog` 下会被解析器**静默忽略**,导致 backend 回落 candle、启动时因找不到本地 mmBERT 模型而崩溃(`FileNotFound("models/mmbert-embed-32k-2d-matryoshka/config.json")` 即此坑)。

```yaml
global:
  model_catalog:
    embeddings:
      semantic:
        embedding_config:
          backend: openai_compatible
          model_type: remote
          target_dimension: 1024
          enable_soft_matching: false   # 强烈建议显式关闭,见第 4 节
        endpoint:
          base_url: http://embedding-service:8000/v1   # OpenAI 兼容 /v1/embeddings
          model: BAAI/bge-m3
          api_key_env: EMBEDDING_API_KEY               # 从该环境变量读取 key
          timeout_seconds: 5
          max_retries: 2
          dimensions: 1024
```

### 3.2 端点契约

服务按 OpenAI embeddings 协议调用:

```
POST {base_url}/embeddings
{"model": "BAAI/bge-m3", "input": ["text1", "text2"], "dimensions": 1024}

→ {"data": [{"index": 0, "embedding": [...]}, ...], ...}
```

- `input` 恒为数组;启动预热阶段逐条候选调用,运行期每请求一条查询
- **硬校验**:`endpoint.dimensions` 必须等于 `embedding_config.target_dimension`
- key 从 `api_key_env` 指定的环境变量读取(如 `EMBEDDING_API_KEY`),本地测试可用占位值
- 可用环境变量 `EMBEDDING_BACKEND` 覆盖配置中的 backend(candle/openvino/openai_compatible)

### 3.3 行为说明

- `backend: openai_compatible` 时**启动跳过本地模型加载**(无需 mmBERT 文件)
- 远程嵌入同时驱动 **embedding 信号评估**与 **selection 算法(router_dc/hybrid 等)的 query 向量**
- 远端不可达:启动预热失败会导致服务无法就绪;运行期调用失败该请求返回 500(不降级到 keyword)

### 3.4 内置嵌入(candle 后端,CPU 推理)

```yaml
global:
  model_catalog:
    embeddings:
      semantic:
        qwen3_model_path: /models/qwen3-embedding-0.6b   # Qwen/Qwen3-Embedding-0.6B 模型目录
        mmbert_model_path: ""   # 显式置空!否则使用内置默认路径并因文件缺失而启动失败
        use_cpu: true
        embedding_config:
          model_type: qwen3
          target_dimension: 1024   # Qwen3-Embedding-0.6B 原生维度(MRL 支持 32~1024)
          enable_soft_matching: false
```

- 模型文件需要自备(config.json / model.safetensors / tokenizer.json 等,约 1.2GB),服务不自动下载
- 换嵌入模型**无需重新训练**;embedding 信号的 candidates 即语义原型,启动时重新预计算
- 已知限制:selection 侧 qwen3 向量走 `GetEmbeddingBatched`,当前启动流程未调用 batched 初始化(启动日志出现 `batched embedding model not initialized`);静态选择不受影响,配置 router_dc/hybrid 等 selection 算法前需先修复

---

## 4. 阈值调优指南

### 4.1 匹配逻辑

```
score = max/加权(各候选与查询的余弦相似度)     # cosine,向量已归一化
硬匹配:score >= 规则 threshold                  # embedding_classifier_scoring.go
软匹配(仅 enable_soft_matching=true 时):score >= min_score_threshold(规范默认 0.5)
```

**注意**:`enable_soft_matching` 规范默认开启。真实模型对短文本(如问候语)的基线相似度不低,
软匹配会把无关请求放进低分决策。生产配置建议**显式 `enable_soft_matching: false`**,
让每条信号的 threshold 成为唯一门限。

### 4.2 不同模型的分数分布不同(实测)

| 嵌入来源 | 相关查询典型分 | 无关查询基线 | 建议阈值起点 |
| --- | --- | --- | --- |
| 确定性 mock(词元哈希) | 0.89~0.92 | ~0.0-0.1 | 0.45 |
| Qwen3-Embedding-0.6B(真实) | 0.79~0.96 | 0.596(hello world) | 0.6~0.7 |

换嵌入模型后 threshold **不可直接复用**,必须按新模型分布重新校准。

### 4.3 调优流程

1. 部署后先看信号评分日志(INFO 级,含每条规则的真实分数):
   ```
   Rule "code_request_embedding": score=0.7943 best=0.8512 threshold=0.600 matched=true
   ```
2. 用 3~5 条**改述过的**正例 + 明显无关例(hello world 等)采样,找到正例最低分与无关例最高分之间的空隙
3. threshold 取空隙中点;若空隙过窄,优先**补充口语化/改述 candidates**(实测:补 2 条口语候选后,
   改述查询分数从 0.615 提升到 0.794,margin 从 0.02 拉开到 0.2),而不是硬压阈值
4. 多候选聚合默认取原型加权最优;`aggregation_method: mean` 改为均值,对候选数量敏感度更高
5. 分不清先看 `matched_rules` 前缀:`embedding:` 命中走语义,`keyword:` 走正则

### 4.4 与 keyword 信号的取舍

| | keyword(regex) | embedding |
| --- | --- | --- |
| 延迟 | ~0-1ms | 本地 CPU 100~200ms / 远程 +网络 RTT |
| 召回 | 字面命中 | 语义泛化(改述、换说法) |
| 依赖 | 无 | 嵌入模型(本地文件或远程服务) |
| 建议 | 高频确定词先上 keyword | 意图类规则用 embedding,阈值按 4.3 校准 |

---

## 5. 部署

### Docker

```bash
docker build -t decision-server:latest .
docker run -p 8080:8080 -e EMBEDDING_API_KEY=xxx \
  -v $PWD/config/decision-server.yaml:/etc/decision-server/config.yaml \
  decision-server:latest
```

### Kubernetes(k3s 实测通过)

- 配置走 ConfigMap 挂载到 `/etc/decision-server/`,启动参数 `-config` 指向所选文件,切换配置即切换决策模式
- 内置模式模型目录用 hostPath 挂载(如 `/models/qwen3-embedding-0.6b`),`nodeSelector` 固定到模型所在节点
- readinessProbe 用 `GET /ready`(周期 1s);镜像已含 `libgomp`,CPU 推理无需 GPU
- 镜像内嵌本文档,`kubectl exec` 或 Service 端口 `curl /docs` 即可查阅

### 启动参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `-config` | `config.yaml` | 路由配置文件路径 |
| `-listen` | `:8080` | HTTP 监听地址 |

---

## 6. 性能参考(k3s 单副本,loadgen c=8/60s,NodePort)

| 指标 | keyword 基线 | 外部嵌入(mock,0ms) | 外部嵌入(mock,100ms) | 内置 Qwen3-0.6B(CPU) |
| --- | --- | --- | --- | --- |
| ready 时间 | ~1.7s | <1s | <1s | ~5s(含模型加载) |
| 内存 RSS | 14Mi | 21Mi | ~21Mi | ~2.5GB |
| CPU @c8 | 5.3 核 | ~11.8 核 | — | ~11.4 核 |
| RPS | 14,573 | 1,681 | 见实测补充 | 15.5 |
| 延迟 p50 | 0.50ms | 3.09ms | ≈100ms 量级 | 509ms |
| 延迟 p99 | 1.51ms | 19.5ms | — | 718ms |

- 外部嵌入模式的吞吐上限 ≈ `并发数 / (嵌入延迟 + 本决策开销)`;c=8、嵌入 100ms 时约 75~80 RPS
- 内置模式单请求 CPU 推理 100~230ms,8 并发下线程争用推高 p50 到 ~500ms;提高吞吐需横向扩副本
- keyword 模式适合与 embedding 混布:确定词先短路,降低嵌入调用比例

---

## 7. 故障排查

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 启动崩溃:`FileNotFound("models/mmbert-embed-32k-2d-matryoshka/config.json")` | 嵌入配置写在了顶层 `model_catalog`(被忽略),backend 回落 candle | 移到 `global.model_catalog.embeddings.semantic` |
| 启动崩溃:同上但配置了 `backend: openai_compatible` | backend 未生效(位置错/拼写错) | 同上;或用 `EMBEDDING_BACKEND=openai_compatible` 强制覆盖验证 |
| 启动日志 `batched embedding model not initialized` | selection 侧 qwen3 向量走 batched 接口但未初始化 | 静态选择可忽略;用 router_dc/hybrid 前需修复 |
| 无关请求被路由到某个决策 | `enable_soft_matching` 默认开启,0.5 兜底生效 | 显式 `enable_soft_matching: false`,按 4.3 重校准 |
| 改述查询全部落到 default | 阈值过高或 candidates 覆盖不足 | 补口语化候选(实测最有效),再微调阈值 |
| decide 返回 500 且日志有 embed 调用失败 | 远程嵌入不可达/超时 | 检查 `base_url` 连通性与 `timeout_seconds`;启动预热失败会直接不就绪 |
