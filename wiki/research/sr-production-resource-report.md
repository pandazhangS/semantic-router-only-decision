# 原始 SR 路由服务生产问题报告:性能瓶颈与 CPU/GPU 资源矛盾

> 2026-09-02。本报告是 decision-server 裁剪优化调研线的**问题陈述收口**:
> 系统化总结原始 vLLM Semantic Router(内置模型形态)在生产中的性能问题,以及
> 路由信号计算与 LLM/嵌入推理之间的 CPU/GPU 资源需求矛盾。
> 结论与解法见:[domain-to-kb-signal-migration.md](domain-to-kb-signal-migration.md)、
> [decision-server-optimization.md](decision-server-optimization.md)、指南 [kb-domain-signal-guide.md](../guides/kb-domain-signal-guide.md)。
> 所有量化数据均标注出处;标注「估算」的为推理值,其余为实测/上游文档数据。

## 1. 原始形态:单容器三合一

原始 SR 镜像在**一个进程**里同时承担三类负载特征完全不同的工作:

| 负载 | 内容 | 时延量级 | 资源画像 |
| --- | --- | --- | --- |
| ① 数据面代理 | ExtProc gRPC、协议解析、决策组合、头改写 | 微秒级 | 纯 CPU,随 QPS 线性 |
| ② 路由信号推理 | 分类器(domain/PII/越狱/复杂度等 BERT 前向)+ 内置嵌入 | 毫秒~十毫秒级 | CPU/GPU 均可,随 QPS |
| ③ LLM/嵌入推理面 | 后端 vLLM、外部嵌入服务 | 秒级(生成)/十毫秒级(嵌入) | GPU 密集,随 token 数 |

canonical 默认配置捆绑 **4 份 mmBERT-32K 权重**(embed/intent/PII/jailbreak,
各 ≈1.2GB fp32,见 §5-1),加上 candle/onnx/openvino/nlp 多套进程内 FFI 绑定。

**矛盾根源:① 的容量公式本应是 `CPU ≈ RPS × 0.4ms`,② 把它污染成 `8~12ms/req`;
② 与 ③ 的负载曲线(随 QPS vs 随 token)不同,却常被绑在同一资源域里。**

## 2. 问题清单

### P1 信号推理吞吐瓶颈(高计算)

- 本地 BERT 分类前向 **7.7~12.3ms/req,占分类总耗时 >99%**(上游 #2951 实测)
- 500m CPU limit 下本地分类路径吞吐封顶 ≈ **35~100 RPS**(估算:0.5 核 ÷ 8~12ms);
  同 limit 下纯代理+远程嵌入路径实测 **1,300 RPS**(0.39ms/req,见优化报告 §1.4)
- 域分类每请求都要前向,无跨请求摊销;文本嵌入还无请求级去重,多信号叠加时远程调用成倍增加

### P2 慢启动

- 启动链 = 模型下载(HF 拉取)+ GB 级权重加载 + tokenizer/warmup,叠加在外部依赖上
  (网络/凭据)——上游 #1625:HF_TOKEN 缺失且下载失败时路由**启动崩溃**
- 上游 #2690(Epic,roadmap)明确把「minimize unnecessary model downloads」列为
  首请求耗时优化的核心 scope——官方承认模型依赖是采用与启动摩擦
- K8s 后果:readiness 慢 → 滚动发布/扩缩容窗口拉长;HPA 扩出的新副本要等模型就绪,
  **弹性响应被启动时间下限钳制**

### P3 内存与装箱

- 4 份 mmBERT 驻留 ≈ **4~5GB 权重**(canonical 默认),加上推理运行时工作集
- 256Mi~512Mi 级小 limit 完全不可行;大 request 压低**节点装箱率**——每个路由副本
  都为「大部分时间空转的信号模型」预留内存
- 镜像体积与构建:candle 绑定 + CUDA 依赖树使构建成为发布瓶颈
  (上游 #1236:vllm-sr + extproc 构建 **超 2 小时**)

### P4 CPU/GPU 资源矛盾(核心)

**形态 A:路由内置模型跑 CPU(默认)——CPU 高负载,GPU 闲置**

- 路由副本数 × BERT CPU 前向,信号计算随 QPS 吃满决策面 CPU(见 P1)
- 同时刻,推理面的 GPU(嵌入/LLM)在等待路由决策返回,**GPU duty-cycle 低位空转**
- 规模化时只能横向加路由副本,每副本重复一份 CPU 推理与内存驻留(见 P7)

**形态 B:把路由内置模型上 GPU——固定多占一份 GPU 资源**

- 每个路由副本常驻独占一份 GPU 显存放小分类器;小模型前向在 GPU 上是亚毫秒~毫秒级,
  **利用率极低却 7×24 占位**——大马拉小车
- GPU 显存碎片化:本可用于 LLM KV-cache 的显存被 512-token 小模型切块
- 与推理面(嵌入/LLM)的 GPU 需求高峰直接冲突,扩容 GPU 的理由被稀释

**形态 C:混部互等(用户实测体感「cpu 计算时 gpu 闲置,gpu 高峰时 cpu 等待」)**

- CPU 侧信号计算慢 → 整个路由决策链变长 → GPU 推理面在队头空等(队头阻塞)
- GPU 高峰(大 batch 嵌入/LLM 生成)挤占 PCIe/显存带宽与宿主 CPU(驱动/预处理)→
  CPU 侧信号前向进一步变慢 → **互相放大的反馈环**
- 两类负载峰值时段往往错开(信号随 QPS、LLM 随 token 生成),但捆绑部署只能按
  **峰值之和**规划资源——两边都有结构性浪费

**形态 D:无法独立扩缩**

- 信号负载(随 QPS)与 LLM 负载(随 token)增长曲线不同;捆绑在单一部署单元里,
  HPA 只能整体扩缩,资源画像按最大公约数分配,SLA 互相牵连

### P5 故障域与弹性

- 进程内 cgo/模型推理的崩溃直接打穿数据面;上游 #2441:启动期分类 API 调用触发
  **nil-pointer panic**;#2706:modelruntime 任务 goroutine panic 在配置重载时击穿存活路由
- 模型加载失败、OOM、权重损坏等「非路由故障」与路由可用性强耦合

### P6 运维耦合

- 换模型/调分类器 = 重新构建发布路由镜像;tokenizer 版本、warmup、artifact 校验与
  路由生命周期绑死(#2951 的动机原话:「decoupling model residency, tokenizer versioning,
  warmup, and CPU scheduling from the routing data plane」)
- 模型下载完整性与打包问题:#2531(缺 qwen3/gemma/multimodal 运行时文件)、
  #2172(镜像只带 ONNX 格式模型,缺 model.safetensors)

### P7 多副本重复驻留

- 每个路由副本各驻留一份模型权重与缓存;N 副本 = N × (权重内存 + warmup + 发布同步)
- #2951 提出的反面即正解:「one resident model, one cache, several callers」

## 3. 矛盾本质:三类负载,一种捆绑

```
              ┌─ ① 代理:  µs 级, CPU, 随 QPS ──────────┐
单进程捆绑 ──┼─ ② 信号:  ms 级, CPU/GPU 均可, 随 QPS ─┤→ 资源画像互污染:
              └─ ③ LLM:   s 级, GPU 密集, 随 token ────┘   按峰值之和分配,两边都浪费
```

- ② 的正确归宿是**独立信号推理面**(可 GPU 化、可共享常驻、可缓存),既不该拖累 ① 的
  CPU 容量公式,也不值得在 ③ 的 GPU 上常驻一个 duty-cycle 极低的小模型
- 语义类信号(嵌入/KB 余弦)天然适合「远程推理面 + 本地纯计算」拆分——拆分后本地只剩
  微秒~亚毫秒级向量运算,这正是 #2262(嵌入外置)已落地、#2782(分类外置,路线图)的方向

## 4. 已验证的演进路径(本调研线收口)

| 阶段 | 状态 | 效果(70ms 嵌入延迟实测) |
| --- | --- | --- |
| ① 嵌入远程化(base64) | ✅ 已落地(上游 #2262) | CPU/req 7.0→0.42ms;优化后 **0.43ms/req**,500m 下 1,300 RPS |
| ② domain→KB 信号(本调研) | 📋 方案就绪 | 决策面 **零 cgo/零模型**;镜像去 Rust stage 与 4×1.2GB 权重;内存 14~15Mi;秒级扩缩 |
| ③ domain 分类外置服务(本仓库可选模式) | ✅ 已落地(2026-09-02) | `category_model.protocol: http_classify` + `domain-classifier` 独立镜像 target;决策面可完全去 cgo,分类面 CPU/GPU 异构独立扩缩;上游 #2782 落地后可平滑切换官方后端 |

**终态拓扑**:决策面 = 纯 Go 无状态 CPU 副本(HPA by RPS,容量公式 `CPU ≈ RPS × 0.4ms`);
推理面 = 嵌入 + 分类 + LLM 的 GPU 服务(独立 HPA,独立 SLO)——CPU/GPU 各按各的
负载曲线规划,互不等待,互不空转。

## 5. 数据出处

| # | 数据/论断 | 出处 |
| --- | --- | --- |
| 1 | mmBERT-base 307M 参数/1.2GB fp32;canonical 捆 4 份 | [arXiv:2509.06888](https://arxiv.org/abs/2509.06888);`pkg/config/canonical_defaults.go` |
| 2 | BERT 前向 7.7~12.3ms/req,占 >99%;gRPC 一跳 22µs | 上游 [#2951](https://github.com/vllm-project/semantic-router/issues/2951) 实测 |
| 3 | 0.39~0.43ms/req;500m 下 1,300 RPS 封顶;优化链 | [decision-server-optimization.md](decision-server-optimization.md) §1 |
| 4 | 嵌入远程化特性与动机 | 上游 [#2262](https://github.com/vllm-project/semantic-router/issues/2262)(已完结) |
| 5 | 首请求耗时/模型下载摩擦 | 上游 [#2690](https://github.com/vllm-project/semantic-router/issues/2690) |
| 6 | 构建超 2h | 上游 [#1236](https://github.com/vllm-project/semantic-router/issues/1236) |
| 7 | 启动崩溃/panic/打包问题 | 上游 [#1625](https://github.com/vllm-project/semantic-router/issues/1625)、[#2441](https://github.com/vllm-project/semantic-router/issues/2441)、[#2706](https://github.com/vllm-project/semantic-router/issues/2706)、[#2531](https://github.com/vllm-project/semantic-router/issues/2531)、[#2172](https://github.com/vllm-project/semantic-router/issues/2172) |
| 8 | 轻量分类替代(GLiClass 吞吐/精度) | [arXiv:2508.07662](https://arxiv.org/abs/2508.07662) |
| 9 | 500m 下本地分类 35~100 RPS 封顶 | 估算:0.5 核 ÷ (8~12)ms,与 #2951、出处 3 交叉一致 |
