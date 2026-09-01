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
          encoding_format: base64   # 可选;响应约 1/4 体积并跳过浮点文本解析,服务端忽略该参数时自动回落浮点数组
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

| 指标 | keyword 基线 | 外部嵌入(70ms 典型) | 内置 Qwen3-0.6B(RAYON=8) |
| --- | --- | --- | --- |
| ready 时间 | ~1.7s | <1s | ~5s(含模型加载) |
| 内存 RSS | 14Mi | ~15Mi | ~2.5GB |
| CPU 稳态 | 5.3 核 @14,573 RPS | 0.07 核 @c=8 / 0.23 核 @c=32 | 8.3 核 |
| RPS | 14,573 | 111 @c=8 / 445 @c=32 | 17.6 |
| 延迟 p50 | 0.50ms | 71.8ms | 449ms |
| 延迟 p99 | 1.51ms | 73.3ms | 557ms |

- 外部嵌入模式吞吐 ≈ `并发 / 嵌入延迟`;端到端延迟由嵌入服务决定;服务自身 CPU ≈ RPS × 0.42ms(base64 后)
- 内置模式单请求 CPU 推理 100~230ms,8 并发下线程争用推高 p50 到 ~450ms;提高吞吐需横向扩副本
- keyword 模式适合与 embedding 混布:确定词先短路,降低嵌入调用比例
- 镜像 447MB(单 cgo 镜像,keyword/远程嵌入配置共用);内置嵌入模式已移除(见 §8)

---

## 7. 性能调优与 pprof

### 7.1 采样工作流

启动时加 `-pprof` 开放采样端点(生产默认关闭):

```bash
# CPU 火焰定位(压测负载中采样 30s)
go tool pprof -top -nodecount=25 'http://<svc>/debug/pprof/profile?seconds=30'
# 堆
go tool pprof -top -inuse_space 'http://<svc>/debug/pprof/heap'
```

### 7.2 已验证的优化

- **远程嵌入连接池(2026-09-01)**:默认 `http.Transport` 的 `MaxIdleConnsPerHost=2`
  导致并发下大多数请求重新 dial+DNS(pprof 实测 91.7% CPU 在连接建立)。已为
  `openai_compatible` provider 内建连接池(`MaxIdleConnsPerHost=100`):真实 70ms 场景
  CPU/req 7.0→0.50ms(−93%);饱和探测下 RPS ×2.9、p99 −81%
- **`encoding_format: base64`(2026-09-01)**:端点按 OpenAI 协议标准返回 LE float32
  二进制,响应 21KB→~5.5KB,跳过浮点文本解析。饱和探测 RPS +13%、CPU/req −15%;
  服务端忽略该参数时自动回落浮点数组,兼容非标准实现;示例配置已默认启用
- 详见 `wiki/research/decision-server-optimization.md`;自建 provider 注入 `HTTPClient` 时同理

### 7.3 内置模式的 CPU 旋钮:RAYON_NUM_THREADS

candle 推理走 Rust rayon **进程级全局**线程池,默认 worker 数 = 全部核数;并发推理共享争抢,
上下文切换/缓存失效会放大总工作量(实测 0.73 → 0.47 核·s/req)。实测(c=8):

| RAYON_NUM_THREADS | RPS | p50 | p99 | CPU |
| --- | --- | --- | --- | --- |
| 默认(16 核机=16) | 15.5 | 509ms | 718ms | 11.4 核 |
| 8 | **17.6** | **449ms** | **557ms** | **8.3 核** |
| 4 | 10.7 | 746ms | 845ms | 4.45 核 |
| 2 | 5.8 | 1363ms | 1491ms | 2.3 核 |

选型:worker ≈ 常态并发 × 单请求可占核数,与部署 CPU 配额对齐。rayon 池是**进程级全局**的,
`RAYON_NUM_THREADS` 是延迟↔CPU 的直接权衡旋钮;容器建议同时把 `GOMAXPROCS` 与 CPU limit 对齐。

### 7.4 GOMAXPROCS:跟随 pod CPU limit

- **延迟主导场景(典型,≥50ms 嵌入)完全不敏感**:70ms 嵌入下 GOMAXPROCS=1/4/宿主默认
  全部 111 RPS,CPU 0.06~0.075 核——I/O 等待不占 CPU,调它没有意义
- 高 RPS 场景(低延迟后端/0ms 饱和探测)超配有代价:16 P(宿主 16 核默认)vs 4 P,
  总 CPU 4.32→3.12 核,GC 标记占比 12.3%→4.4%——超配的 P 是 GC 并行标记与调度同步的放大器
- `GOMAXPROCS=1` 对 candle 模式毫无影响(推理走 rayon 原生线程池,不经 Go 调度器)
- 生产:GOMAXPROCS 跟随 pod CPU limit(automaxprocs),不是宿主核数;失配会触发 CFS throttling

### 7.5 吞吐量纲速查

- 远程嵌入:吞吐 ≈ `并发 / 嵌入延迟`;路由服务自身 ~0.42ms CPU/req(base64 后),水平扩副本线性扩展
- 内置嵌入:CPU/req 随线程数与模型大小变化,常驻内存 ~2.5GB(Qwen3-0.6B)
- keyword:0.36ms CPU/req,无优化必要

### 7.6 资源水位与 limit/request(cgroup v2 实测,70ms 典型)

| 水位 | 场景 | RPS | CPU | 内存 |
| --- | --- | --- | --- | --- |
| 空闲 | 无流量 | 0 | 0.001 核 | RSS 8Mi |
| 典型 | 70ms 嵌入延迟,c=8 | 111 | 0.075 核 | RSS ~15Mi |
| 扩展 | 70ms 嵌入延迟,c=32 | 445 | 0.23 核 | RSS ~15Mi |
| 饱和上限探测 | 0ms mock 压满 CPU 路径,c=32 | 8,757 | 3.32 核(GOMAXPROCS=4 封顶) | peak 19.8Mi |

- 容量公式:CPU ≈ RPS × 0.5ms;嵌入延迟主导(≥50ms)时单副本 CPU 需求通常 <0.1 核
- 建议:request `cpu 250m / memory 64Mi`,limit `cpu 4 / memory 256Mi`;
  limit cpu 与 GOMAXPROCS 对齐(GOMAXPROCS ≤ limit,避免 CFS throttling 抬延迟)

---

## 8. 二开:不用内嵌模型时裁剪 cgo

只保留 **远程嵌入 + keyword(regex)+ 静态选择** 的部署,可以把 candle-binding / nlp-binding
两个 Rust cgo 依赖整体从构建链里拿掉——**无需删除任何调用点代码**。

### 8.1 原理:binding 自带纯 Go 编译桩

两个 binding 均内置 `windows || !cgo` 编译桩:

- `candle-binding/semantic-router_mock.go`
- `nlp-binding/nlp_binding_mock.go`

`CGO_ENABLED=0` 时 Go 自动选择桩实现(所有 FFI 函数返回 `ErrBackendUnavailable`),
`go build` 直接通过,二进制不含 cgo、不需要任何 `.so`。

### 8.2 裁剪后的能力边界

| 能力 | cgo 构建 | CGO_ENABLED=0 |
| --- | --- | --- |
| 远程嵌入信号(openai_compatible) | ✓ | ✓ |
| keyword 信号 `method: regex` | ✓ | ✓ |
| 静态/elo 等非 ML 模型选择 | ✓ | ✓ |
| 内置嵌入(candle 后端) | ✓ | ✗(backend 返回错误) |
| mmBERT 分类器信号(domain/PII/jailbreak 等) | ✓ | ✗ |
| keyword `method: bm25/ngram` | ✓ | ✗ |
| 多模态图像嵌入 / NLI | ✓ | ✗ |

### 8.3 构建差异

```bash
# 本地
cd src/semantic-router && CGO_ENABLED=0 go build -o decision-server ./cmd/decision-server
```

Dockerfile 简化(删 Rust stage 与 .so 拷贝):

```dockerfile
FROM golang:1.25-bookworm AS build
ENV CGO_ENABLED=0 GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY . .
RUN cd src/semantic-router && go build -o /out/decision-server ./cmd/decision-server

FROM debian:bookworm-slim
COPY --from=build /out/decision-server /usr/local/bin/decision-server
COPY config/decision-server.yaml /etc/decision-server/config.yaml
EXPOSE 8080
ENTRYPOINT ["decision-server"]
```

收益:构建不再需要 Rust 工具链(阶段 1 整体去掉,CI 时间大幅缩短)、镜像更小、
二进制静态可移植、无 `.so` 加载面。

### 8.4 验证清单

1. 启动后 `GET /ready` 正常(远程 backend 启动本就跳过本地模型加载)
2. embedding 信号命中正常(`matched_rules` 带 `embedding:` 前缀)
3. keyword 正则命中正常;若配置了 `bm25/ngram` 会显式报错(桩返回错误),属预期
4. 错误信息形如 "candle backend unavailable",用于发现误配置了 `backend: candle`

### 8.5 进一步瘦身(激进,可选)

若连桩代码也不想要:删掉 `candle-binding/`、`nlp-binding/` 两个 module,并替换上文中
19 个调用点文件(`pkg/classification/*.go` 为主,加 `cmd/decision-server/main.go`、
`pkg/modelruntime/router_runtime.go`)里的直接调用为本地接口。**不推荐**——桩方案零维护,
收益只剩几 MB 源码;调用点手术的 review 成本远高于此。

---

## 9. 故障排查

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 启动崩溃:`FileNotFound("models/mmbert-embed-32k-2d-matryoshka/config.json")` | 嵌入配置写在了顶层 `model_catalog`(被忽略),backend 回落 candle | 移到 `global.model_catalog.embeddings.semantic` |
| 启动崩溃:同上但配置了 `backend: openai_compatible` | backend 未生效(位置错/拼写错) | 同上;或用 `EMBEDDING_BACKEND=openai_compatible` 强制覆盖验证 |
| 启动日志 `batched embedding model not initialized` | selection 侧 qwen3 向量走 batched 接口但未初始化 | 静态选择可忽略;用 router_dc/hybrid 前需修复 |
| 无关请求被路由到某个决策 | `enable_soft_matching` 默认开启,0.5 兜底生效 | 显式 `enable_soft_matching: false`,按 4.3 重校准 |
| 改述查询全部落到 default | 阈值过高或 candidates 覆盖不足 | 补口语化候选(实测最有效),再微调阈值 |
| decide 返回 500 且日志有 embed 调用失败 | 远程嵌入不可达/超时 | 检查 `base_url` 连通性与 `timeout_seconds`;启动预热失败会直接不就绪 |
