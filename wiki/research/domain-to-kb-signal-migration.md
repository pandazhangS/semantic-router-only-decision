# domain 信号去本地化:KB 替代方案调研(CPU/GPU 分离部署视角)

> 2026-09-02。结论先行:**domain 信号可由 KB 信号(远程嵌入 + 余弦打分)替代,且是官方一等公民路径**;
> 替代后 decision-server 全链路零 cgo、零本地模型权重,决策面与推理面(嵌入/LLM)彻底分离,
> 不再为每次请求 7~12ms 的本地 BERT 前向买单。

## 1. 问题:domain 信号的隐性成本

生产预览版启用的信号中(长文本 structure / domain / projection / 难度 complexity / keyword / 多因子),
**只有 domain 一项依赖 cgo 本地推理**,但它带来四重成本:

| 成本维度 | 具体数字 | 出处 |
| --- | --- | --- |
| CPU/req | 本地 ModernBERT 前向 **7.7~12.3ms,占分类总耗时 >99%** | 上游 #2951 实测(同数量级,单核) |
| 吞吐封顶 | 500m limit 下本地分类 ≈ **35~100 RPS**;对比 keyword+远程嵌入路径 0.39ms/req → 1,300 RPS | 前者按 0.5 核 ÷ 8ms 估算;后者见 [decision-server-optimization.md](decision-server-optimization.md) |
| 内存 | 分类模型权重数百 MB 级(fp32 BERT 系),当前 256Mi limit 必然 OOM,镜像需内置模型 | registry(512 ctx)与 Dockerfile(运行时镜像无模型目录) |
| 构建 | Rust(candle + CUDA 依赖树)编译是镜像构建大头;CI 曾报 vllm-sr+extproc 超 2h | 上游 #1236;本地 Dockerfile stage 1 |

对比:其余信号全部纯 Go——keyword(regex)/structure(token 计数、regex 特征)/
complexity(远程嵌入 + 余弦原型库)/projection(加权算术)/多因子选择器(`ExternalDependencies()` 返回空)。

## 2. 上游 issue 证据链:官方同样在甩掉内嵌模型

| Issue | 状态 | 要点 |
| --- | --- | --- |
| [#2262](https://github.com/vllm-project/semantic-router/issues/2262) external embedding for signal logic | ✅ 已完结 | 动机:内置嵌入性能问题 + 生产应使用专用推理运行时。6 个 PR 全落地(#2270 CRD、#2276 provider 抽象——**远程模式不下载不初始化本地嵌入模型**、#2390 扩展到 embedding/KB/complexity/对比分类全部文本嵌入消费方)。**已部署的 openai_compatible 远程嵌入即此特性的产物** |
| [#2951](https://github.com/vllm-project/semantic-router/issues/2951) out-of-process classification (llm-d-sc) | 已关闭→转 #2782 | 直接论证进程内 FFI 绑定之重:解耦 model residency/tokenizer 版本/warmup/CPU 调度;gRPC 一跳仅 22µs(0.18%),模型前向占 >99%。维护者承认「domain 分类器暂无稳定外置集成计划」 |
| [#2782](https://github.com/vllm-project/semantic-router/issues/2782) typed Router Model deployment contracts | 开放 Epic(accepted) | 上游战略答案:本地+远程部署统一 typed 契约、远程共享 outbound connector。**方向与 KB 替代一致,但远程分类后端尚未落地** |
| [#2690](https://github.com/vllm-project/semantic-router/issues/2690) Reduce time to first routed request | 开放 Epic(roadmap) | scope 第一条「minimize unnecessary model downloads」——官方承认模型下载是采用摩擦 |
| [#2966](https://github.com/vllm-project/semantic-router/issues/2966) WG Router Models & Inference Runtime | 进行中 | 模型与推理运行时解耦的专项工作组 |

外围佐证:#2531(下载完整性缺 candle 文件)、#2172(镜像只带 ONNX 格式模型)、
#1236(构建超 2h)、#2668(声明远程后端却被静默用 Candle)、#2926(ONNX Arm64 轻量探索)。

**判断:嵌入侧官方已彻底转向远程(#2262);分类侧外置化在路线图上(#2782)但未落地——
KB 信号是当下唯一不依赖本地模型的官方领域分类路径。**

## 3. KB 替代的可行性(本仓库代码验证)

### 3.1 官方现成模板

`config/recipes/knowledge/`(README + config.yaml + probes.yaml + recipe.dsl)是完整的
「KB 做领域证据路由」官方 recipe,Requirements 只要求 **OpenAI-compatible endpoint + mmlu_kb 资产文件**,
全篇无本地模型。`config/recipes/privacy/` 同样以 privacy-KB 做隐私/安全分类。

### 3.2 decision-only 构建链已支持

- `buildKBClassifiersOption`(pkg/classification/classifier_option_rules.go)→
  `NewKnowledgeBaseClassifierWithProvider`,嵌入 provider 即远程 openai_compatible
- 启动时把 exemplars 预编码为原型库(prototype 聚类,max_prototypes/margin/best_weight 可调),
  请求时 query embedding 对原型库余弦打分
- 决策 DSL:`type: kb`(kind: label/group,match: best)、投影输入 `type: kb_metric`(group_margin)、
  `type: projection` 条件、keyword 边界修正——全部在 decision-server 可用
- 现成资产:`config/knowledge_bases/mmlu/labels.json`(14 个 MMLU 域标签 × ~15 exemplars)、
  `config/knowledge_bases/privacy/labels.json`

### 3.3 替换形态与代价

```
domain(本地 ModernBERT 前向, cgo, 7~12ms CPU)  →  kb(远程嵌入 + 余弦, 纯 Go, ~0.1-0.3ms CPU)
```

- 每请求 +1 次远程嵌入调用:延迟 +70ms 量级(与现有 semantic 嵌入同水位);CPU 增量可忽略
- **文本嵌入请求级去重:已落地(2026-09-02)**——`requestTextEmbeddingCache`
  (pkg/classification/request_text_embedding_cache.go)在 EvaluateAllSignalsWithContext
  入口分配,semantic/complexity/KB 三路对同一请求文本的远程嵌入合并为一次调用
  (sync.Once 单飞,并发信号安全)。只覆盖远程 provider 路径,本地后端(openvino/candle)
  因各分类器 modelType 不同不入缓存;**刻意不做进程级缓存**——无跨请求状态,零泄露风险。
  编排级验证:TestEvaluateAllSignalsWithContext_DedupsTextEmbeddingAcrossSignals

## 4. CPU/GPU 分离部署:生产可用性收益

替代后的资源拓扑——**决策面只做字符串与向量运算,一切模型推理外移推理面**:

| 维度 | 现状(domain 本地) | KB 替代后 |
| --- | --- | --- |
| 决策面 CPU/req | 0.4ms + 7~12ms(分类) ≈ **8~12ms** | **~0.4ms**(+KB 余弦 <0.3ms) |
| 决策面内存 | 256Mi 不可行(权重数百 MB) | 14~15Mi RSS 维持 |
| 镜像 | 需内置/挂载模型 + 两个 .so | 447MB → 纯 Go 静态二进制,cgo/模型层全删 |
| 扩缩容 | HPA 受模型 warmup 与大内存拖累 | 无状态纯 CPU,按 RPS 线性扩,秒级拉起 |
| 推理面 | GPU 空转,分类挤占决策面 CPU | 嵌入(bge-m3)/LLM 全部收敛到 GPU 推理服务,资源按各自水位独立规划 |
| 故障面 | 模型加载失败/OOM/分类器 panic 均打穿数据面 | 决策面故障域缩小;嵌入服务故障走显式超时/降级策略 |
| 构建发布 | Rust 编译阶段 + 模型同步 | CGO_ENABLED=0,单 stage Go 构建,秒级缓存命中 |

生产可用性核心论点:**决策面的容量公式回归 `CPU ≈ RPS × 0.4ms`、`吞吐 ≈ 并发 / 嵌入延迟`**,
不再被本地推理的 token 数敏感项污染;嵌入服务(可 GPU 化)独立扩缩,SLO 单独治理。

### 4.1 可选模式:domain 分类外置服务(已落地 2026-09-02)

除 KB 替代外,本仓库提供第二条解耦路径——**把 Rust/candle 的 domain 分类拆成独立服务**,
决策面(CPU)与分类面(CPU/GPU)异构部署,互不等待、各自按负载曲线独立扩缩:

**路由侧配置**(`category_model.protocol` 切换,与本地 candle 路径互斥):

```yaml
category_model:
  model_id: mmbert32k-intent-classifier   # 逻辑名(远程模式下不加载本地权重)
  protocol: http_classify                 # 切换到外置服务
  threshold: 0.55
  category_mapping_path: /app/config/category_labels/mmlu_labels.json
external_models:
  - name: domain-classifier-svc
    llm_provider: classifiers
    model_role: classification            # 路由侧按此角色查找端点
    llm_model_name: domain-classifier
    llm_endpoint:
      address: domain-classifier-svc      # 独立服务地址(k8s Service 名)
      port: 8090
      protocol: http
```

**独立服务**(Dockerfile target `domain-classifier`,复用 Rust 编译层缓存):

```bash
docker build --target domain-classifier -t domain-classifier:latest .
docker run -p 8090:8090 \
  -v /path/to/category-model:/models/domain \
  -v /path/to/category_labels.json:/config/category_labels.json \
  domain-classifier:latest -model /models/domain -mapping /config/category_labels.json
```

**约束与行为**:
- 外置服务必须返回**完整 label 分布**(按 `category_mapping_path` 名称对齐,
  复用 jailbreak http_classify 的同一契约与校验器 `alignScoresToMapping`,score 和须 ≈1.0)
- candle FFI 仅 ModernBERT / Candle-BERT 路径提供概率输出(mmBERT-32K intent 无 WithProbs
  变体)→ 服务以 `-variant modernbert|candle_bert` 加载对应格式模型
- 远程模式不创建本地 initializer、不做模型加载(短路,同 jailbreak);构造期校验
  fail-closed:协议未知 / 缺 classification 角色端点 / 缺 mapping → 启动硬失败
- 错误语义与本地路径一致:分类失败 → domain 信号报错跳过,不阻塞其它信号
- 部署形态:路由镜像可完全去 cgo(`CGO_ENABLED=0`),分类服务独享 GPU/CPU,
  独立 HPA/SLO——CPU 决策面 0.4ms 容量公式与 GPU 分类面各按各的水位规划

**与 KB 方案的关系**:KB = 零新增模型(复用嵌入面 bge-m3,exemplar 迭代);
外置服务 = 保留训练好的 domain 分类头(有标注精度上限),只是换部署位置。
两者都让决策面摆脱 cgo 与模型权重,按精度需求与运维偏好选择,也可共存
(KB 做主信号、外置分类做对照)。

## 5. 落地清单

1. **配置迁移**:decision-server 配置删 `domain` 信号 → 按 knowledge recipe 增加
   `routing.signals.kb`(kind: label/group)+ 决策条件 + 可选 `kb_metric` 投影
2. **KB 资产校准**:用生产 query 样本(目标嵌入模型 bge-m3)重算 exemplars 相似度分布,
   重定 threshold(参考:mmlu_kb 0.25 / privacy_kb 0.55 是按上游嵌入模型标的,不可直接照抄);
   exemplars 用真实领域样本迭代,每标签 ≥10 条
3. **文本嵌入去重**(**已落地** 2026-09-02):请求级 `requestTextEmbeddingCache` 单飞缓存,
   semantic/complexity/KB 复用同一次调用,消除多信号叠加的重复 RTT;
   仅请求级、无进程级缓存(避免泄露),见 §3.3
4. **cgo 退场**(残留判定清单):
   - **candle-binding**:domain→KB 后生产信号集零消费者,可整体删除(连同 Dockerfile Rust stage)
   - **nlp-binding**:仅当 keyword 规则存在 `method: bm25/ngram` 时残留——KB 是语义信号,
     不覆盖词法打分,两者互补而非替代。全部 regex(当前 decision-server.yaml 即是)即可删
   - 若要「bm25/ngram 也要 + cgo 清零」:纯 Go 重写是最薄的一块(Rust 侧仅 bm25/ngrammatic
     两个纯算法 crate,无推理框架无权重),`compressor_nlp_test.go` 的整套一致性测试可作验收契约
   - 行为安全:非 cgo 构建 + bm25/ngram 规则 = **启动硬失败**
     (mock `AddRule` 报错 → `NewKeywordClassifier` 报错 → `buildKeywordClassifierOption`
     返回 err → 构建中止),不存在规则静默失效
5. **验证**:
   - 契约/单测:KB 信号 + 投影 + 决策组合(config validator 已覆盖)
   - 压测:**一律 70ms 真实延迟 mock**(0ms mock 无效,见优化报告头部规范),
     对比 domain 本地模式与 KB 模式的 RPS/CPU/限流曲线,验证 §4 容量公式
   - 准确率:用生产 query 回放对比 domain 分类 vs KB 打分的路由一致率,输出混淆矩阵定阈值

## 6. 风险与开放问题

- **路由一致率**:KB 是最近邻证据打分,不是训练分类头;边界域(math vs physics 类)靠
  keyword 边界修正与 margin 阈值兜底,knowledge recipe 即此模式
- **嵌入延迟成为新依赖**:嵌入服务抖动直接进入路由关键路径——需要超时/降级决策
  (fail-open 到默认模型 or fail-closed,按业务定性)
- **exemplar 治理**:KB 会随业务漂移老化(上游 recipe README 明示 stale KB 风险),
  需要把 labels.json 纳入版本化流程与定期回归(probes.yaml 模式)
- **上游演进**:#2782 落地后可能出现官方远程分类后端,届时 KB 方案与之兼容
  (决策 DSL 不变,仅信号实现替换)

## 7. 附:mmBERT 依赖重量与轻量替代全景

### 7.1 mmBERT 到底有多重

[mmBERT](https://arxiv.org/abs/2509.06888)(2025-09)base 版总参数 **307M(非嵌入仅 110M)**,
因 256K 多语词表(Gemma 2 tokenizer)嵌入矩阵占 ~197M;fp32 单份 ≈ **1.2GB**。

本仓库 canonical 默认配置捆绑 **4 份** mmBERT-32K 模型驻留
(pkg/config/canonical_defaults.go 与 canonical_loader_test.go):

| 模型 | 用途 |
| --- | --- |
| `models/mmbert-embed-32k-2d-matryoshka` | 内置嵌入(已随远程化移除) |
| `models/mmbert32k-intent-classifier-merged` | domain/category 分类 |
| `models/mmbert32k-pii-detector-merged` | PII 检测 |
| `models/mmbert32k-jailbreak-detector-merged` | 越狱检测 |

即完整本地形态 ≈ **4~5GB 权重 + 进程内 Rust 推理**,而生产预览实际只用到其中 domain 一份——
「为一类信号驻留一整套多语编码器」正是本调研要消除的结构性浪费。

### 7.2 轻量替代对比(GLiClass 为 2025-08 zero/few-shot 分类 SOTA 候选)

数据来源:GLiClass 论文 [arXiv:2508.07662](https://arxiv.org/abs/2508.07662)
(吞吐为 A6000 GPU 单卡 batch=1;CPU 部署需另行压测):

| 方案 | 参数量/体积 | 精度 | 延迟特征 | 数据需求 | 部署形态 |
| --- | --- | --- | --- | --- | --- |
| **mmBERT-base trained head**(现状) | 307M / 1.2GB fp32 | 有标注数据时精度上限最高 | 7.7~12.3ms CPU/req(#2951) | 需训练集 | 进程内 cgo |
| **KB 质心/原型**(复用 bge-m3) | **0 新增模型** | 取决于 exemplar 质量,可迭代 | RTT(70ms)+ 余弦 <0.3ms | ≥10 exemplars/标签 | 远程嵌入 |
| GLiClass-edge-v3.0 | 32.7M / **131MB** | zero-shot F1 0.49;8-shot **+50% rel** | 97 ex/s | 8 条/标签 | 远程推理面 |
| GLiClass-base-v3.0 | 187M / 746MB | F1 0.6764(与 deberta-v3-large zero-shot 0.6821 差 0.006) | 52 ex/s | 同上 | 远程推理面 |
| GLiClass-large-v3.0 | 439M / 1.75GB | F1 0.7193(超 deberta-v3-large zero-shot) | 25 ex/s | 同上 | 远程推理面 |
| 同模型 ONNX/OpenVINO int8 量化 | 权重 ÷2~4 | ≈ 无损 | CPU 前向 2~4× 提速 | 无(复用现有权重) | 进程内(上游 openvino-binding;本 fork 已 stub) |
| fastText / 线性探针 | <10MB | 粗粒度域分类可用 | <0.1ms | 需标注训练集 | 任意 |

标签扩展性(域标签增长的场景):GLiClass 1→128 标签吞吐仅降 **-20%**;
cross-encoder(NLI 对)同场景恶化 **52×**——多标签域分类选单前向架构而非对爆炸是硬约束。

### 7.3 分场景结论

- **决策面去重(本调研主线)**:KB 仍是唯一「新增模型成本为零」的方案——bge-m3 已在推理面驻留,
  domain/PII/越狱全部改 KB/规则后,mmBERT 4 份驻留整体退场
- **KB 精度不足时**:在推理面部署 GLiClass(few-shot 8 条/标签即可大幅提升小模型),
  按上游 #2782「attached-service」形态作为远程分类后端接入,决策面仍是纯 Go
- **仅在「必须进程内本地」的约束下**:才考虑保留 mmBERT 但换 ONNX/OpenVINO int8 量化
  (精度近似、CPU 前向 2~4× 提速),这是工程优化而非性质改变——架构性浪费(4 份驻留)仍在
