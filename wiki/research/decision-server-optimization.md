# decision-server 性能优化报告

> 2026-09-01,k3s 单节点(16C)实测。**所有压测数据一律来自 70ms 外部嵌入延迟(典型生产值)mock**;
> 0ms mock 压测无意义(嵌入模式端到端由嵌入延迟主导,只会产出虚高吞吐与失真结论),不采用。
> 每种模式各自以**无优化初始版本**为基线,逐项给出收益;末列给出全量应用后的终态与总收益率。
> 复现工具:decision-server `-pprof` 端点 + loadgen(固定并发)+ 容器 cgroup v2 计数器
> (cpu.stat 差分 / memory.peak / nr_throttled 限流差分)。

## 1. 外部嵌入模式:优化链

### 1.1 基线(无优化,初始版本)

| 指标 | 70ms 典型(c=8) |
| --- | --- |
| RPS | 111.2 |
| 延迟 p50 / p99 | 71.83ms / 74.15ms |
| CPU | ~0.8 核(7.0ms/req) |
| 内存 RSS | 14Mi |

### 1.2 逐项收益(每项在前一项基础上)

| # | 优化项 | 改动 | 之前 → 之后(70ms 实测) | 该项收益 |
| --- | --- | --- | --- | --- |
| 1 | 远程嵌入连接池 | 默认 Transport 只留 2 条空闲连接(并发下 91.7% CPU 在 dial),内建 `MaxIdleConnsPerHost=100` | CPU/req 7.0ms → 0.50ms | **CPU/req −93%**;吞吐/延迟不变(被嵌入延迟主导) |
| 2 | GOMAXPROCS 对齐 limit | 500m quota 下宿主默认 4 P → G=1 | c=8 CPU 0.070→0.048 核;c=32 0.224→0.175;c=64 0.435→0.345 | **CPU −21~32%**,限流周期 181→1,p99 79.3→76.4ms |
| 3 | Go 工具链 1.25→1.27 | Dockerfile 构建镜像升级 | 无感(±2% 噪声) | 保留为工具链升级 |
| 4 | encoding_format=base64 | 响应 LE float32 二进制(21KB→5.5KB),双格式解码保兼容 | 服务侧 CPU/req ~0.50→0.42ms | CPU/req −15%;端到端无感(嵌入主导) |
| 5 | GOGC=300 实验 | — | 无收益(±2% 噪声;GC 占比已低) | 撤销 |

剩余热点为正常 HTTP 服务形态(syscall/GC/JSON 解串/cosine 各占 <20%,无单一异常热点)。
cgo 评估:远程热路径无 FFI 调用(嵌入走 HTTP、keyword 走 regex/bm25/ngram),
**保留 cgo 零性能代价**(cgocall 开销仅存在于内置模式,已随 §5 移除)。

### 1.3 全部应用后的终态与总收益率(GOMAXPROCS=1,limit 500m)

| 指标 | 基线(无优化) | 终态(全量优化) | 总收益率 |
| --- | --- | --- | --- |
| 70ms 典型(c=8) | 111.2 RPS,p50 71.83ms,CPU 0.8 核(7.0ms/req) | 111.3 RPS,p50 71.82ms,**CPU 0.048 核(0.43ms/req)** | **CPU/req −94%**,延迟不变(嵌入主导) |
| 70ms 扩展(c=32) | — | 445 RPS,p50 71.56ms,CPU 0.175 核(0.40ms/req) | 线性扩展验证 |
| 70ms 边缘(c=64) | — | 890 RPS,p50 71.56ms,CPU 0.345 核 | 500m quota 余量 31% |
| 内存 RSS | 14Mi | 14~15Mi | 持平 |

容量公式:**CPU ≈ RPS × 0.4ms;单副本吞吐 ≈ 并发 / 嵌入延迟**。

### 1.4 CPU limit 下限验证(2026-09-01 补充)

目标:压掉 limit 冗余,验证最小 CPU limit 且无性能损失(70ms mock,每轮 60s,
cgroup `cpu.stat` 的 usage/throttled 差分):

| limit / GOMAXPROCS | c=8 | c=32 | c=64 | c=128 |
| --- | --- | --- | --- | --- |
| 无限制 / 4(原状态) | 111.3 RPS / 0.070 核 | 444.9 / 0.219 核 | — | — |
| 1C / 4 | 111.3 / 0.070 核 / 0 限流 | 445.2 / 0.223 核 / 0 限流 | — | — |
| 500m / 4 | 111.3 / 0.069 核 / 0 | 445.5 / 0.224 核 / 0 | 890.5 RPS / 0.435 核 / p99 79.3ms / 181 周期限流 | **1,293 RPS 钉死在 quota**,p50 99 / p99 172ms |
| 500m / 1(**终态**) | — | 444.8 / 0.175 核 / 0 | 890.0 RPS / 0.345 核 / p99 76.4ms / 1 周期 | — |
| 250m / 1 | 111.3 / 0.048 核 / 0 | 445.5 / 0.178 核 / p99 76.0ms / 19 周期(累计 0.08s) | — | — |

- **1C 与 500m 零性能损失**:c=8/32/64 的 RPS、p50/p99 与无限制完全一致;**250m 在 c≤32 同样可用**(轻微限流但无感)
- 500m quota 吞吐封顶 ≈ **1,300 RPS**(= 0.5 核 ÷ 0.39ms/req,需 ~91 并发在飞);超过后排队劣化但平缓(延迟抬升、无错误)
- 小 quota 下超配 P 是真实开销(GC 并行标记 + 调度自旋被放大):500m 下 G=4→G=1 CPU −21~32%、限流 181→1 周期
- 部署终态:request `250m/64Mi`,limit `500m/256Mi`,GOMAXPROCS=1(c=32 占 quota 35%,c=64 占 69%);
  预算紧张且 ≤445 RPS 的场景可降到 250m

## 2. 内置嵌入模式(candle CPU 推理):优化链

> 该模式已随 §5 裁剪移除(数据为移除前实测,保留作对照)。
> 镜像:447MB 单 cgo 镜像,keyword/远程嵌入配置共用(`-config` 切换);mock-embedding 123MB 仅测试床使用。

### 2.1 基线(无优化,默认参数)

15.5 RPS,p50 509ms,p99 718ms,CPU 11.4 核(0.73 核·s/req),RSS ~2.5GB,ready ~5s。

### 2.2 逐项收益

| # | 优化项 | 改动 | 之前 → 之后(c=8) | 该项收益 |
| --- | --- | --- | --- | --- |
| 1 | RAYON_NUM_THREADS=8 | worker 数对齐并发数(rayon 默认按全部核数建进程级全局池) | 15.5 RPS/509ms/718ms/11.4 核 → 17.6 RPS/449ms/557ms/8.3 核 | **吞吐 +14%,CPU −27%,p50 −12%,p99 −22%**,CPU/req 0.73→0.47 核·s(−36%) |

机制(线程级证据):16 个 rayon worker 各 0.60~0.67 核(利用率 ~2/3)——8 个请求在飞、
16 个 worker 抢活;worker=并发数时全员满负荷,总 CPU 11.4→8.3 核。
pprof:81.1% 在 `[libcandle_semantic_router.so]`、9.2% `libc.so.6`、9.2% `runtime.cgocall`;
Go 侧调用链完整,.so 内部 Rust 函数不可见(native 帧无法展开)。

### 2.3 终态与总收益率

RAYON=8 即当前全部优化:**吞吐 +14%、CPU −27%、p50 −12%**;默认参数下仍可按
"RAYON_NUM_THREADS ≈ 常态在飞并发推理数"取值。遗留:selection 侧 batched 初始化缺陷
(静态选择不受影响);进一步方向:embed 层 batch 化、ONNX 量化。

## 3. keyword 模式(regex)

基线即终态:14,573 RPS @5.3 核,p50 0.50ms,0.36ms CPU/req——无优化必要。
适合与 embedding 混布:确定词先短路,降低嵌入调用比例。

## 4. 逐项细节与证据(按优化项)

### 4.1 连接池(优化项 #1,已合入 bbfc368d)

默认 `http.Transport` 的 `MaxIdleConnsPerHost=2`:并发下连接全部短命,每请求重新
dial+DNS。pprof 实测(压测负载中 30s 窗口采样)`dialConnFor` 占 91.75%,
`runtime.allocm` 因大量 dial 阻塞 goroutine 膨胀。修复为 provider 内建共享 Transport
(`MaxIdleConns: 200, MaxIdleConnsPerHost: 100, IdleConnTimeout: 90s`)后 dial 从热路径消失。

### 4.2 GOMAXPROCS(优化项 #2)

- RPS/延迟对 GOMAXPROCS 不敏感:嵌入延迟主导下 I/O 等待不占 CPU,G=1/4/宿主默认吞吐全同
- 但 **CPU 用量敏感,limit 收口后尤其明显**:500m quota 下 G=4 vs G=1(70ms 实测)
  c=64 CPU 0.435→0.345 核、限流周期 181→1、p99 79.3→76.4ms——4 个 P 挤 0.5 核,
  GC 并行标记(gcDrain)与调度自旋(futex/tgkill)被小 quota 放大成净开销
- 生产:GOMAXPROCS = pod CPU limit 向上取整(500m→1、1C→1),不是宿主核数;
  失配既触发 CFS throttling 又浪费 CPU

### 4.3 base64(优化项 #4,已合入 7372cc74)

OpenAI embeddings 协议 `encoding_format: "base64"`(LE float32):响应 21KB→~5.5KB,
跳过浮点文本解析与 float64 中间分配(改动前的最大分配项)。provider 双格式
解码:服务端忽略该参数返回浮点数组时自动回落,兼容不破坏;非法取值启动即报错。
正确性:base64 与浮点路径对同一请求 confidence 逐位一致(0.916926171630621)。

### 4.4 cgo 内部热点怎么采(方法论)

- Go pprof:定位到 FFI 边界(哪个 FFI 调用、哪条 Go 路径、哪个 .so);native 内部不可见
- OS 级:`perf record -g -p <pid>`(需 perf_event_paranoid≤2 或 root)、Windows ETW、macOS Instruments
- Rust 侧:cargo flamegraph;FFI 内部分段打点
- 零权限粗粒度:/proc/<pid>/task/*/stat 双快照差分(CLK_TCK=100,内置模式分析所用)

## 5. 内置嵌入后端裁剪(cgo 保留)

- **保留**:cgo 单镜像(Rust .so 随镜像分发)——mmBERT 分类器信号
  (PII/jailbreak/fact-check/feedback/modality)、bm25/ngram keyword、多模态图像嵌入不变
- **移除**:内置文本嵌入后端(candle)——`InitEmbeddingModels` 本地模型加载、
  selection 的 candle 向量分支全部删除;embedding 信号与 selection 的向量一律来自
  openai_compatible 远程端点;误配 backend: candle 启动即报错(取代 FileNotFound 崩溃),
  部署不再需要 qwen3/mmbert 本地模型文件与 hostPath 挂载
- 纯 Go 编译桩(CGOC_ENABLED=0 可构建)同步修复至与上游 API 对齐,本地快速编译可用;
  bm25/ngram 与分类器在该模式下返回 unavailable(能力边界,预期行为)
- 遗留:selection 侧 batched 初始化缺陷随本地向量路径移除而失效;内置模式如需恢复,
  回退对应提交即可

## 6. 采集方法

- CPU/heap:`go tool pprof -top 'http://<svc>/debug/pprof/profile?seconds=30'`(压测中采样)
- 稳态资源:容器 cgroup v2(`cpu.stat` usage_usec 差分、`memory.current/.peak`、
  `nr_throttled/throttled_usec` 限流差分);容器内 `kubectl exec ... -- cat /sys/fs/cgroup/cpu.stat` 即可读
- 线程级:/proc/<pid>/task/*/stat 双快照差分
- limit 下限验证:`kubectl set resources` → rollout → c=8/32/64 各 60s,
  对比 RPS/p50/p99 与限流差分,零损失即通过

## 7. 终态对比:优化前内置嵌入 vs 调优后外部嵌入

> 内置模式数据为裁剪前实测(模式已随 §5 移除,当时与外嵌共用同一镜像、`-config` 切换);
> 外嵌终态 = §1 全部优化 + limit 500m / GOMAXPROCS=1。

| 指标 | 内置嵌入基线(candle,无优化) | 内置优化后(RAYON=8) | **外嵌终态(全量优化)** | 基线 → 终态收益 |
| --- | --- | --- | --- | --- |
| 吞吐 RPS(c=8) | 15.5 | 17.6 | **111.3** | **×7.2** |
| 吞吐扩展 | — | — | 445 @c=32 / 890 @c=64 / ~1300 封顶 | 最高 **×84** |
| 延迟 p50 | 509ms | 449ms | **71.8ms** | **−86%** |
| 延迟 p99 | 718ms | 557ms | **73.3ms** | **−90%** |
| CPU 总量(c=8) | 11.4 核 | 8.3 核 | **0.048 核** | **−99.6%** |
| CPU/req | 730ms | 470ms | **0.43ms** | **−99.94%(≈1700×)** |
| 内存 RSS | ~2.5GB | ~2.5GB | **14~15Mi** | **−99.4%** |
| 单核吞吐 | 1.4 RPS/核 | 2.1 RPS/核 | **~2500 RPS/核**(445 RPS/0.175 核) | **≈1200~1800×** |
| ready 时间 | ~5s | ~5s | **<1s** | −80% |
| 单副本 CPU 需求 | ≥8C | 8C | **500m(零损失验证,§1.4)** | **−94%** |
| 承载 890 RPS 所需 | ~51 副本 ≈ 420 核(优化后口径) | 同左 | **1 副本 × 0.345 核** | **≈1200× CPU 效率** |

根因:内置模式每个请求在路由进程内做一次 CPU 前向推理(100~230ms CPU/req),吞吐被推理
串行化、并发争用推高延迟;外嵌模式把推理移交专职嵌入服务,路由侧只剩 HTTP 调用 + cosine
(0.4ms CPU/req),延迟由网络 RTT 与嵌入服务决定(70ms),CPU 随 RPS 线性、副本水平扩展
(HPA 友好,见 api_guide §10 之后的部署建议)。两模式共用同一 447MB 镜像,镜像大小无差异;
裁剪只删除了内置嵌入的代码路径,其余决策模式不受影响(能力矩阵见 api_guide §10)。
