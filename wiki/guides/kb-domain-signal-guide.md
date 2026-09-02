# KB 信号替换 mmBERT domain:落地与调优指南

> 2026-09-02。回答两个问题:**怎么把 domain(本地 mmBERT/cgo)换成 KB 信号**,以及**换完之后每个配置项怎么调**。
> 为什么换见 [domain-to-kb-signal-migration.md](../research/domain-to-kb-signal-migration.md);
> 官方参考实现:`config/recipes/knowledge/`(escalate/keep 模板)与 `config/recipes/privacy/`。
> 本文所有行为描述均核对过代码路径,关键位置随文标注。

## 1. 机制速览(调参的前提)

KB 分类的完整数据流:

```
labels.json (每标签 description + exemplars)
   │  首次使用时批量嵌入(8 并发 worker,每条 exemplar 一次远程调用)
   ▼
每标签原型库 prototypeBank(相似 exemplar 聚成 ≤ max_prototypes 个质心)
   │  请求: query → provider.Embed(1 次远程调用)
   ▼
score(label) = best_weight × best + (1−best_weight) × mean(top_m 余弦)
   │  score ≥ threshold(或 label_thresholds 覆盖) → label matched
   ▼
signal 匹配(kind: label/group,match: best/threshold) + kb_metric 投影值
```

| 环节 | 代码位置 |
| --- | --- |
| KB 构建与远程 provider 接线 | `pkg/classification/classifier_option_rules.go` `buildKBClassifiersOption` |
| manifest 加载与路径解析 | `pkg/config/kb_config.go` `LoadKnowledgeBaseDefinition` |
| exemplar 并行嵌入 + 原型库重建 | `pkg/classification/category_kb_embeddings.go` |
| 打分(余弦/best/support) | `pkg/classification/prototype_scoring.go` `prototypeBank.score` |
| 阈值与匹配判定 | `pkg/classification/category_kb_scoring.go` |
| 信号绑定(kind/match) | `pkg/classification/classifier_signal_taxonomy.go` |

**group 分数 = 成员标签分数的 max**;**group_margin = positive_group 分数 − negative_group 分数**。
`BestLabelMargin = 最高分 − 次高分`,是模糊边界的核心观测量。

## 2. 三层配置解剖

### 2.1 KB 资产:`knowledge_bases/<name>/labels.json`

```json
{
  "version": "1.0.0",
  "description": "生产领域分类 KB(替换 mmBERT domain)",
  "labels": {
    "coding": {
      "description": "编程/技术问题",
      "exemplars": ["...≥10 条真实 query..."]
    }
  }
}
```

- **exemplars 为 0 的标签被静默跳过**(`loadDefinition`),不会报错——务必校验标签数
- `version` 自己维护,配合发布流程做回归

### 2.2 KB 实例声明 + 信号绑定(`routing` 层)

```yaml
model_catalog:            # KB 实例(可多个)
  knowledge_bases:
    - name: domain_kb
      source:
        path: knowledge_bases/domain/   # 相对 config 文件目录,或绝对路径
        manifest: labels.json
      threshold: 0.45                  # 全局默认阈值
      label_thresholds:                # 按标签覆盖
        general_chitchat: 0.55
      prototype_scoring:
        enabled: true
        cluster_similarity_threshold: 0.9
        max_prototypes: 8
        best_weight: 0.75
        top_m: 2
        margin_threshold: 0.0
      groups:                          # 标签 → 决策组(组分 = max)
        technical: [coding, database, devops]
        business: [finance, legal]
      metrics:                         # 命名数值指标 → 投影
        - name: tech_vs_business
          type: group_margin
          positive_group: technical
          negative_group: business

routing:
  signals:
    kb:
      - name: domain_coding            # 信号名(决策引用它)
        kb: domain_kb
        target: { kind: label, value: coding }
        match: best                    # 或 threshold(默认)
  decisions:
    - name: technical_lane
      priority: 100
      rules:
        conditions:
          - type: kb
            name: domain_coding
        operator: OR
      modelRefs: [...]
```

### 2.3 `match` 语义表(选错是最常见的坑)

| match | kind: label | kind: group |
| --- | --- | --- |
| `best` | 该标签是**全 KB 最高分**才命中(不看阈值)——等价于分类器 argmax | 该组是**所有组中最高分**才命中 |
| `threshold`(默认) | 该标签分数过阈值即命中(可多标签同时命中) | 组内任一标签过阈值即命中 |

**替换 domain 的直觉映射**:mmBERT 分类头是 argmax 语义 → 用 `match: best`;
但 argmax 无阈值兜底,**建议 AND 一个 threshold 模式信号或 margin 检查**防止「最高分但绝对值很低」的垃圾命中。

### 2.4 投影输入(`type: kb_metric`)

`metric` 可用:`group_margin`(需 positive/negative_group)、内置 `best_score`、`best_matched_score`;
投影里 `value_source: score`。escalate/keep 模式见 knowledge recipe 的 `escalation_pressure` 投影。

## 3. 迁移步骤(五步)

1. **定 taxonomy**:把 mmBERT 的 category mapping 域列表抄成 KB 标签;同时决定分组
   (决策只需要组级区分时,标签可以比原 14 类更细——组吸收粒度)
2. **写 labels.json**:每标签 ≥10 条 exemplar(见 §4.1),来源优先级:生产 query 抽样 > 人工编写 > MMLU 种子
3. **配 KB + 信号 + 决策**:按 §2.2;先影子配置(新 decisions 用低 priority 叠加),不删 domain
4. **回放对比**:同一批生产 query 分别走 domain 与 KB 路径,输出混淆矩阵与路由一致率(§6)
5. **切换与下线**:一致率达标后删 `domain` 信号——`classifier.category` 运行时任务是
   `usesRoutingSignalType(domain) && IsCategoryEnabled()` 门控的,信号删掉即不再加载 mmBERT,
   启动日志应消失 `classifier.category` 初始化事件

## 4. 调优手册

### 4.1 exemplar 治理(决定上限的一层)

| 原则 | 说明 |
| --- | --- |
| 数量 | 每标签 10~20 条;<8 条阈值不稳,>30 条边际收益低(原型库会聚掉) |
| 多样性 | 覆盖子话题与措辞变体;**故意放边界样本**(和邻近标签只差一个关键词的) |
| 干净度 | 删近似重复(会被 cluster 吸收浪费 max_prototypes 名额);跨标签不得互相泄漏 |
| 语言 | bge-m3 跨语对齐好,中英混合 exemplar 可行;但**校准集必须与生产语言分布一致** |
| 长度 | exemplar 与生产 query 长度分布对齐,避免「exemplar 全是长文档、query 全是短句」的分布漂移 |

### 4.2 threshold 校准(核心工序)

mmlu_kb 的 0.25 / privacy_kb 的 0.55 是**上游嵌入模型标的,换 bge-m3 必须重标**。流程:

1. 每标签收集 30~50 条「应命中」生产 query(正例)+ 50 条「不应命中」(负例,含邻近域)
2. 打分脚本:对每条 query 调 `/v1/embeddings`,与该标签 exemplars 算原型库分数(复刻 §1 公式)
3. 看两条分布:正例分数 p5 与负例 p95,**取两者之间的谷**为 threshold;
   分不开的标签先修 exemplar,别硬调阈值
4. 全局 `threshold` 放宽、`label_thresholds` 收紧难标签;chitchat/兜底类标签阈值应**更高**
   (防垃圾命中),专业窄域可略低
5. 上线后观测 `SignalConfidences` 分布(`kb:<rule>` 键)与 `BestLabelMargin`,持续微调

经验起点:bge-m3 归一化向量下,域分类正例余弦多落在 0.5~0.7、跨域负例 0.3~0.45,
**全局 0.45~0.5 起步**,按分布法修正——不要照抄任何上游数值。

### 4.3 prototype_scoring 逐参数

| 参数 | 默认 | 语义 | 调法 |
| --- | --- | --- | --- |
| `cluster_similarity_threshold` | 0.9 | exemplar 相似度 ≥ 此值并入同一原型(质心) | 标签内部话题杂 → 降到 0.85(更多原型);想压缩 → 0.95 |
| `max_prototypes` | 8 | 每标签原型上限 | 多模态标签(如 coding 含前后端/算法)可到 12;一般 8 够 |
| `best_weight` | 0.75 | score = w×best + (1−w)×top_m 均值 | exemplar 高度同质 → 0.9;标签宽泛靠「多条都像」支撑 → 0.6 |
| `top_m` | 2 | support 取前 m 个原型均值 | 宽标签 3~4;窄标签 2 足够 |
| `margin_threshold` | 0.0 | 原型库内部 margin(日志观测用) | 保持 0,margin 治理放到信号层做 |

调参顺序:**先 exemplar → 再 threshold → 最后 prototype_scoring**;原型参数只解决「标签内分布形状」,
救不了 exemplar 质量问题。

### 4.4 分组与升级型决策

- 组分 = max(成员标签分),组的意义是**决策粒度与标签粒度解耦**:决策写 `kind: group`,
  标签随便细化
- 升级/降级型路由(小模型 vs 大模型)用 `group_margin` 投影 + `threshold_bands`
  (knowledge recipe 的 `escalation_pressure` 模式),比布尔信号平滑
- `metrics` 里的 `group_margin` 正负组必须是**同一 KB 的两个 groups**

### 4.5 keyword 边界修正(knowledge recipe 必学)

KB 是最近邻语义,系统性边界错误(如「矩阵求逆」总被吸进 physics)用 keyword 信号做确定性覆盖:

```yaml
decisions:
  - name: escalate_math
    rules:
      operator: AND
      conditions:
        - operator: OR
          conditions:
            - { type: kb, name: domain_math }
            - { type: keyword, name: math_boundary_override }
        - operator: NOT
          conditions:
            - { type: keyword, name: math_exclusion }
```

原则:**keyword 修正 KB,不替代 KB**;每次加 override 都要在 probes 里固化用例。

### 4.6 嵌入端点配套

- `encoding_format: base64` 对 KB 同样生效(exemplar 预载与 query 走同一 provider)
- KB 对嵌入端点的调用模式:**启动后首次使用 = N 条 exemplar 的突发并发(8 worker)**,
  端点要有并发余量;`timeout_seconds`/`max_retries` 沿用全局 endpoint 配置
- exemplar 嵌入与 query 嵌入必须是**同一模型同一维度**——换嵌入模型 = KB 全量重校准

## 5. 冷启动与性能(重要代码事实)

- `shouldDeferPreload()` 在**远程 provider 模式下恒为 true**
  (`category_kb_classifier.go`:`backend=="" || backend=="candle" || provider!=nil`),
  即 **decision-server 启动时不预载 exemplars**;`PreloadKnowledgeBases()` 虽存在
  (`classifier.go`/`recipe_classifiers.go`)但 **cmd/decision-server 未接线**
- 后果:**首个 KB 请求会同步嵌入全部 exemplars**(8 并发)。
  预算公式:`冷启动 ≈ Σ_labels exemplars × RTT ÷ 8`。
  例:14 标签 × 15 条 × 70ms ÷ 8 ≈ **1.8s 的首个请求 stall**;KB 大了之后(1000 条)≈ 9s
- 对策(按推荐序):
  1. **部署后 warmup**:启动探针后立刻打一条带 KB 信号的探测请求(或直接调分类 API),把预载做掉
  2. **接线预载**:在 `buildRouter` 里调用 `classifiers.PreloadKnowledgeBases()`
     (best-effort,失败只告警)——上游已有现成方法,一行接线
  3. 稳态开销:每请求 1 次 query 嵌入(70ms RTT)+ 余弦(<0.3ms CPU),
     多 KB 会叠加调用次数——**同一 KB 不要既做 label 信号又做 group 信号之外再拆多个 KB 实例**

## 6. 验证与回归

| 手段 | 做法 | 通过线(建议) |
| --- | --- | --- |
| 静态 | `vllm-sr validate` / decision-server 启动日志 | `knowledge_base_classifier_initialized` 出现,无 `create_failed` |
| 离线回放 | 生产 query 样本(每标签 ≥30)走 domain 与 KB 双路径 | 路由一致率 ≥95%,不一致样本人工归因 |
| 混淆矩阵 | KB 打分 vs 人工标注域 | 每标签 precision/recall ≥ 目标(如 0.9) |
| probes 固化 | 仿 `config/recipes/knowledge/probes.yaml`,每个标签 + 边界 + override 各 1 条 | 全绿,纳入 CI/发布检查 |
| 压测 | 70ms 真实延迟 mock(禁 0ms),KB 模式 vs domain 模式 | 容量符合 `CPU ≈ RPS × 0.4ms`,无本地分类的 8~12ms/req |
| 回归 | labels.json `version` 变更 → probes + 回放子集全跑 | 全绿才发布 |

## 7. 运维陷阱清单

| 陷阱 | 后果 | 防护 |
| --- | --- | --- |
| exemplar 数为 0 的标签 | **静默跳过**,标签消失 | 发布脚本校验 manifest 标签数 |
| KB 路径解析失败 | `knowledge_base_classifier_create_failed` + **kb 信号整体静默禁用**(`kb_signals_enabled: false` 只打告警日志) | 启动后 grep 告警;引用该信号的决策应显式校验 |
| 路径解析顺序 | baseDir(config 同目录)→ `/app/config` → 源码树 config/ → `VLLM_SR_CONFIG_ASSET_ROOT` | k8s 里把 KB 目录与 config 同 ConfigMap/Volume 挂载,别依赖回退 |
| 换嵌入模型 | 全部分数分布漂移,阈值作废 | 嵌入模型版本纳入 KB 资产版本约定,换模型 = 全量重校准 |
| match: best 无阈值 | 低分 argmax 也会命中 | best 信号 AND 一个 threshold 模式信号 |
| 首请求冷启动 | 见 §5 | warmup 或接线预载 |
| KB 过时 | 语义漂移命中率下降 | 每季度用新生产样本回放;probes 常绿 |

## 8. 一页速查

```yaml
# 决策面零 cgo 的 domain 替代最小配置
model_catalog:
  knowledge_bases:
    - name: domain_kb
      source: { path: knowledge_bases/domain/, manifest: labels.json }
      threshold: 0.45            # 用分布法标定,勿抄上游
      prototype_scoring:
        enabled: true
        max_prototypes: 8
        best_weight: 0.75
      groups:
        technical: [coding, database, devops]
routing:
  signals:
    kb:
      - name: is_technical
        kb: domain_kb
        target: { kind: group, value: technical }
        match: threshold
  decisions:
    - name: technical_lane
      priority: 100
      rules:
        conditions: [{ type: kb, name: is_technical }]
      modelRefs: [...]
```

 checklist:exemplar ≥10/标签 → 分布法标阈值 → probes 固化 → warmup 预载 → 删 domain 信号 →
 确认启动日志无 `classifier.category` → mmBERT 4 份驻留退场。
