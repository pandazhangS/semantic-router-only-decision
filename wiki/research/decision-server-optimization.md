# decision-server 性能优化报告

> 2026-09-01,k3s 单节点(16C)实测。**结论以 70ms 外部嵌入延迟(典型生产值)为主线**;
> "0ms mock"数据仅作为饱和上限探测(压满 CPU 路径),单独标注。
> 每种模式各自以**无优化初始版本**为基线,逐项给出收益;末列给出全量应用后的终态与总收益率。
> 复现工具:decision-server `-pprof` 端点 + loadgen(固定并发)+ 容器 cgroup v2 计数器。

## 1. 外部嵌入模式:优化链

### 1.1 基线(无优化,初始版本)

| 指标 | 70ms 典型(c=8) | 0ms 饱和探测(c=8) |
| --- | --- | --- |
| RPS | 111.2 | 1,681 |
| 延迟 p50 / p99 | 71.83ms / 74.15ms | 3.09ms / 19.5ms |
| CPU | ~0.8 核(7.0ms/req) | 11.8 核(7.0ms/req) |
| 内存 RSS | 14Mi | 14Mi |

### 1.2 逐项收益(每项在前一项基础上)

| # | 优化项 | 改动 | 生效场景 | 之前 → 之后 | 该项收益 |
| --- | --- | --- | --- | --- | --- |
| 1 | 远程嵌入连接池 | 默认 Transport 只留 2 条空闲连接(91.7% CPU 在 dial),内建 `MaxIdleConnsPerHost=100` | 70ms 典型 | CPU/req 7.0ms → 0.50ms | **CPU/req −93%**;吞吐/延迟不变(被嵌入延迟主导) |
| | | | 0ms 探测 | 1,681→4,833 RPS;p99 19.5→3.67ms;CPU 11.8→4.6 核 | RPS ×2.9,p99 −81% |
| 2 | GOMAXPROCS=4 | 替代宿主 16 核默认 | 0ms 探测 | 4,833→6,429 RPS;CPU 4.6→3.1 核 | 吞吐 +33%,CPU −48%;**70ms 场景无感**(延迟主导) |
| 3 | Go 工具链 1.25→1.27 | Dockerfile 构建镜像升级 | 0ms 探测 | CPU/req 0.505→0.493ms | 无感(±2%),保留 |
| 4 | encoding_format=base64 | 响应 LE float32 二进制(21KB→5.5KB),双格式解码保兼容 | 0ms 探测 | 6,494→7,307 RPS;p99 3.07→2.78ms;CPU/req 0.493→0.419ms | RPS +13%,CPU/req −15%;**70ms 场景端到端无感** |
| 5 | GOGC=300 实验 | 远程(0ms 探测) | CPU/req 0.419→0.411ms | 无收益(±2% 噪声;GC 占比已低),撤销 |

剩余热点画像(base64 后,0ms 探测,c=8,总 2.76 核):syscall 17%(HTTP 读写,结构性)、
GC+分配 ~13%、JSON 解串 ~10%、cosineSimilarity ~5%、调度 ~4%——正常 HTTP 服务形态,
无异常热点。cgo 评估:远程热路径无 FFI 调用(嵌入走 HTTP、keyword 走 regex),
**保留 cgo 零性能代价**(cgocall 开销仅存在于内置模式,已随 §5 移除)。

### 1.3 全部应用后的终态与总收益率

| 指标 | 基线(无优化) | 终态(全量优化) | 总收益率 |
| --- | --- | --- | --- |
| 0ms 探测吞吐(c=8 / c=32) | 1,681 / — | 7,307 / **8,757** | **×5.2(+420%)** |
| 0ms 探测 CPU/req | 7.0ms | 0.419ms(c=8) | **−94%** |
| 0ms 探测延迟 p50/p99(c=8) | 3.09 / 19.5ms | 0.98 / 2.78ms | p99 −86% |
| 70ms 典型(c=8) | 111.2 RPS,p50 71.83ms,CPU ~0.8 核 | 111.3 RPS,p50 71.84ms,**CPU 0.072 核** | **CPU −91%**,延迟不变(嵌入主导) |
| 70ms 扩展(c=32) | — | 445 RPS,p50 71.69ms,CPU 0.229 核 | 线性扩展验证 |
| 内存 RSS | 14Mi | ~15Mi | 持平 |

容量公式:**CPU ≈ RPS × 0.42ms;单副本吞吐 ≈ 并发 / 嵌入延迟**。
limit/request 建议:request `250m/64Mi`,limit `4C/256Mi`,GOMAXPROCS ≤ limit。

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
dial+DNS。pprof 实测(0ms 饱和探测,30s 窗口 370s 样本)`dialConnFor` 占 91.75%,
`runtime.allocm` 因大量 dial 阻塞 goroutine 膨胀。修复为 provider 内建共享 Transport
(`MaxIdleConns: 200, MaxIdleConnsPerHost: 100, IdleConnTimeout: 90s`)后 dial 从热路径消失。

### 4.2 GOMAXPROCS(优化项 #2)

同负载(0ms 探测)16 P vs 4 P:GC 标记(gcDrain)4.35%→12.33%、调度同步
(futex+tgkill)1.44%→4.30%、madvise 出现 1.04%——**P 数远超实际 CPU 需求时,
GC 并行放大与调度同步成为净开销**。70ms 延迟主导场景 GOMAXPROCS 完全不敏感。
生产建议:跟随 pod CPU limit(automaxprocs),失配触发 CFS throttling。

### 4.3 base64(优化项 #4,已合入 7372cc74)

OpenAI embeddings 协议 `encoding_format: "base64"`(LE float32):响应 21KB→~5.5KB,
跳过浮点文本解析与 float64 中间分配(此前占分配 >55%、CPU ~34%)。provider 双格式
解码:服务端忽略该参数返回浮点数组时自动回落,兼容不破坏;非法取值启动即报错。
正确性:base64 与浮点路径对同一请求 confidence 逐位一致(0.916926171630621)。

### 4.4 cgo 内部热点怎么采(方法论)

- Go pprof:定位到 FFI 边界(哪个 FFI 调用、哪条 Go 路径、哪个 .so);native 内部不可见
- OS 级:`perf record -g -p <pid>`(需 perf_event_paranoid≤2 或 root)、Windows ETW、macOS Instruments
- Rust 侧:cargo flamegraph;FFI 内部分段打点
- 零权限粗粒度:/proc/<pid>/task/*/stat 双快照差分(CLK_TCK=100,本次所用)

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
- 稳态资源:容器 cgroup v2(`cpu.stat` usage_usec 差分、`memory.current/.peak`)
- 线程级:/proc/<pid>/task/*/stat 双快照差分
