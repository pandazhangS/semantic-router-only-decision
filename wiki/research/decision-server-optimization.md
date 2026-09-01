# decision-server 性能优化报告

> 2026-09-01,k3s 测试环境(control-plane 16C/62G)实测。
> 复现工具:decision-server `-pprof` 端点 + `/tmp/decision-test/loadgen-bin`(c=8 并发,NodePort)。

## 1. 背景

三种部署配置的基线压测(c=8/60s)显示 CPU 占用异常偏高,尤其远程嵌入模式:

| 配置(基线) | RPS | p50 | p99 | CPU |
| --- | --- | --- | --- | --- |
| keyword 正则 | 14,573 | 0.50ms | 1.51ms | 5.3 核 |
| 远程嵌入(mock 0ms) | 1,681 | 3.09ms | 19.5ms | **11.8 核** |
| 内置 Qwen3-0.6B(candle CPU) | 15.5 | 509ms | 718ms | **11.4 核** |

远程模式折合 **~7ms CPU/请求**——对一个"HTTP 调用 + 一次 1024 维余弦"的路径明显过量;内置模式折合 ~0.73 核·s/请求。用 pprof 定位。

## 2. 问题一:远程嵌入模式 91% CPU 花在 TCP dial

### 2.1 采样

`go tool pprof -top 'http://<svc>/debug/pprof/profile?seconds=30'`(负载中采样,30.11s 窗口总样本 370s ≈ 12.3 核):

```
   339.81s  91.75%  internal/runtime/syscall.Syscall6
   339.45s  91.65%  syscall.RawSyscall6
   338.76s  91.47%  net/http.(*Transport).dialConnFor        ← 连接建立
   335.23s  90.51%  net.(*netFD).connect
      5.93s   1.60%  encoding/json.(*decodeState).array       ← 真实业务 <3%
      8.78s   2.37%  pkg/embedding.(*OpenAICompatibleProvider).embedBatchOnce
```

heap 侧最大项是 `runtime.allocm`(OS 线程膨胀,大量 dial 阻塞 goroutine 所致),总内存仅 8MB——内存无问题。

### 2.2 根因

`pkg/embedding/openai_provider.go` 构造 `&http.Client{Timeout: ...}` 时使用**默认 Transport**,其 `MaxIdleConnsPerHost=2`:8 并发下每轮只有 2 条连接进入空闲池,其余 6 条被关闭,下一个请求重新走 TCP 三次握手 + DNS 解析(`net.(*Resolver).exchange` 亦在榜上)。连接全部短命,keep-alive 形同虚设。

### 2.3 修复

为 provider 内建共享 Transport,把空闲连接池放大到并发规模:

```go
client = &http.Client{
    Timeout: timeout,
    Transport: &http.Transport{
        Proxy: http.ProxyFromEnvironment,
        DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
        MaxIdleConns:        200,
        MaxIdleConnsPerHost: 100,
        IdleConnTimeout:     90 * time.Second,
        TLSHandshakeTimeout: 10 * time.Second,
    },
}
```

### 2.4 前后对比(mock 0ms,c=8/60s,零错误)

| 指标 | 修复前 | 修复后 | 变化 |
| --- | --- | --- | --- |
| RPS | 1,681 | **4,833** | **×2.9** |
| 延迟 p50 | 3.09ms | **1.56ms** | −50% |
| 延迟 p99 | 19.5ms | **3.67ms** | −81% |
| CPU | ~11.8 核(7.0ms/req) | **~4.6 核(0.95ms/req)** | **−74%/req** |
| 内存 | 14Mi | 14Mi | 持平 |

修复后 pprof 复查 dial 已从热路径消失,剩余热点为 JSON 编解码与余弦计算(正常业务开销)。

## 3. 问题二:内置模式 rayon 线程争用

### 3.1 现象与分析

candle(CPU)单请求推理 ~180ms,但 8 并发下 p50 涨到 509ms、CPU 11.4 核。原因:Rust rayon 默认按"全部核数"建**进程级全局**线程池(本机 16 worker),8 个并发推理共享争抢,上下文切换/缓存失效把单请求总工作量放大(0.73 核·s/req)。`RAYON_NUM_THREADS` 可限制池大小,是延迟与 CPU 的直接权衡旋钮。

### 3.2 实测(c=8/45s)

| RAYON_NUM_THREADS | RPS | p50 | p99 | CPU | CPU/req |
| --- | --- | --- | --- | --- | --- |
| 默认(16) | 15.5 | 509ms | 718ms | 11.4 核 | 0.73 核·s |
| 4 | 10.7 | 746ms | 845ms | 4.45 核 | **0.42 核·s** |
| 8 | **17.6** | **449ms** | **557ms** | **8.3 核** | 0.47 核·s |
| 2 | 5.8 | 1363ms | 1491ms | 2.3 核 | 0.40 核·s |

RAYON=8 全面占优:8 并发 × 8 worker 恰好消除争用且打满 CPU,吞吐 +14%、延迟 -12%~-22%、CPU -27%。worker 数与典型并发对齐(`≈ GOMAXPROCS × 常态并发/并发上限`)是选型思路。

### 3.3 建议

- 追求吞吐/低延迟(独占节点):保持默认 16 线程
- 多副本共址/省 CPU(横向扩容场景):`RAYON_NUM_THREADS=4~8`,单请求 CPU 工作量下降 40%+,代价是单请求延迟上升
- 该旋钮是部署期环境变量,无需改代码;容器建议同时设置 `GOMAXPROCS` 与 CPU limit 匹配

## 4. GOMAXPROCS 实验:单线程/宿主核数该不该设?

针对"单线程能否减少上下文切换、提升吞吐"与"是否应设为宿主核数"两个问题实测(远程嵌入 mock 0ms,c=8):

| GOMAXPROCS | RPS | p50 | p90 | p99 | CPU |
| --- | --- | --- | --- | --- | --- |
| 1 | 2,808 | 2.37ms | 4.44ms | 6.61ms | ~1.0 核 |
| **4** | **6,429** | **1.09ms** | **2.00ms** | **3.17ms** | **3.1 核** |
| 8 | 4,836 | 1.55ms | 2.32ms | 3.58ms | 4.2 核 |
| 默认(=宿主核数 16) | 4,906 | 1.55ms | 2.30ms | 3.54ms | 4.6 核 |

结论:

- **最优值在中间(4),两个极端都次优**:默认(宿主 16 核)比 GOMAXPROCS=4 少 31% 吞吐、
  多 48% CPU——16 个 P 对只需 ~3 核的服务是纯开销(16 个 runqueue/mcache、更分散的 GC);
  单线程则把并行度封顶,损失 42% 吞吐
- 单线程下 CPU/req 略降(0.36ms),但远不足以弥补并行损失;延迟主导场景(70ms 嵌入)时
  GOMAXPROCS=1 与默认完全持平(111 RPS,CPU 需求仅 0.06 核)——I/O 等待不占 CPU,"省切换"无意义
- candle 模式:GOMAXPROCS=1 **毫无影响**(推理在 rayon 原生线程池,不经 Go 调度器),调 RAYON 才有效
- 生产建议:**GOMAXPROCS 跟随 pod CPU limit / 实际负载需求,而非宿主核数**;无 limit 时
  可用 automaxprocs 或压测定值;limit 与 GOMAXPROCS 失配会导致 CFS throttling

## 5. 核数需求归因(2026-09-01 补充采样)

连接池修复后,远程模式仍剩 ~0.5ms CPU/req(GOMAXPROCS=4);内置模式 8~11 核。本轮用
CPU/heap profile、cgroup v2 计数器(cpu.stat/memory.*)与 /proc 线程增量把核数需求拆到函数/线程级。

### 5.1 远程嵌入模式:剩余 CPU 的构成(GOMAXPROCS=4,mock 0ms,c=8)

30s CPU 采样 93.89s(3.12 核),与 cgroup `cpu.stat` 实测 3.28 核吻合:

| 去向 | 占比 | 证据(pprof cum) |
| --- | --- | --- |
| encoding/json 解码嵌入响应 | ~34% | decodeState.array 32.9%、literalStore 22.2%、indirect 12.3% 等 |
| 网络读写 syscall | ~11% | internal/runtime/syscall.Syscall6 flat 11.3% |
| cosineSimilarity(原型评分) | ~4.3% | classification.cosineSimilarity |
| GC(标记 + 写屏障) | ~5.4% | gcDrain 4.35%、wbBufFlush1 1.10% |
| reflect/strconv/每请求 dispatcher 构建等 | 其余 | 解码路径伴生开销 |

分配侧(alloc_space 累计):`reflect.growslice` 44% + `json.Decoder.refill` 11.5%
(JSON 流缓冲)+ `convertEmbedding`(float64→float32)7.2%——**解码 21KB JSON 响应
的路径占分配 >55%,折合 ~150KB/请求**(1024 维 float64 数组按文本解析是天然大户),
是 GC 常驻开销的来源。另有每请求构建信号 dispatcher 闭包数组(~12 个,~5KB/req)。

**结论**:0.5ms/req ≈ 1/3 JSON 解析 + 1/9 网络栈 + 1/10 GC,无异常热点,属正常
HTTP 服务开销。如需继续压缩:OpenAI embeddings 协议支持 `encoding_format: "base64"`,
响应体积减半且跳过 float 文本解析,是性价比最高的下一步(provider 与嵌入服务需同时支持)。

### 5.2 GOMAXPROCS=16(宿主核数)多耗的 ~1.2 核去哪了

同负载下 16 个 P 采样 130.17s(4.32 核)vs 4 个 P 的 93.89s(3.12 核):

| 项(占总 CPU) | GOMAXPROCS=4 | =16 |
| --- | --- | --- |
| GC 标记(gcDrain cum) | 4.35% | **12.33%**(分配量相近,并行扫描/协调放大) |
| 写屏障冲洗(wbBufFlush1) | 1.10% | 2.54% |
| 调度同步(futex + tgkill) | 1.44% | 4.30% |
| madvise(内存归还) | — | 1.04% |

两轮 RPS 相近 → GC 总工作量应相近,但 16 个 dedicated mark worker 把同一份扫描
摊出近 4 倍采样占比。**P 数远超实际 CPU 需求时,GC 与调度器自身成为净开销**——
这就是"宿主核数默认配置比 GOMAXPROCS=4 多耗 48% CPU"的主因。

### 5.3 内置嵌入模式:9~11 核全部来自 rayon 原生线程

candle 模式 30s CPU 采样 275.57s(9.1 核):

| 占比 | 位置 |
| --- | --- |
| 81.1% | `[libcandle_semantic_router.so]`(Rust 推理) |
| 9.2% | `libc.so.6` |
| 9.2% | `runtime.cgocall`(FFI 边界) |

Go 侧调用链完整可见(evaluateEmbeddingSignal → computeEmbedding →
GetEmbeddingWithModelType → _Cfunc_get_embedding_2d_matryoshka),共享库名可定位;
但 .so 内部 Rust 函数对 pprof 不可见(native 帧无法展开,时间归入 _ExternalCode)。

/proc 线程增量(42s 窗口,CLK_TCK=100):**16 个 rayon worker 各 0.60~0.67 核
(利用率 ~2/3),合计 10.6 核;Go 侧全部线程合计 <0.5 核**。

利用率 2/3 的机制:8 个请求在飞、16 个 worker 抢活,平均 1/3 worker 时间无活可干。
这正是 §3.2 中 RAYON_NUM_THREADS=8(=并发数)全面占优的机制解释——worker 对齐
并发数时每个 worker 满负荷,总 CPU 11.4 → 8.3 核。**选型补强:RAYON_NUM_THREADS
≈ 常态在飞并发推理数,不是越大越快。**

### 5.4 cgo 内部热点怎么采(方法论)

- **Go pprof**:能定位到 FFI **边界**——哪个 FFI 调用花多少时间、由哪条 Go 路径
  调用、在哪个共享库(.so 名可见);native 内部函数不可见
- **OS 级采样**:Linux `perf record -g -p <pid>`(需 perf_event_paranoid≤2 或
  root;本测试节点 paranoid=4 且无免密 root,不可用)、Windows ETW/WPR、macOS Instruments
- **Rust 侧**:`cargo flamegraph`(封装 perf);或在 FFI 内部分段打点计时
- **零权限粗粒度**:/proc/<pid>/task/*/stat 双快照增量(本次所用),可归因到线程级

## 6. 资源水位线与 limit/request(外部嵌入模式,cgroup v2 实测)

GOMAXPROCS=4,确定性 mock 为嵌入后端:

| 水位 | 场景 | RPS | CPU | 内存 |
| --- | --- | --- | --- | --- |
| 空闲 | 无流量 | 0 | 0.001 核 | RSS 8Mi |
| 常态 | 70ms 嵌入延迟,c=8 | 111 | **0.075 核** | RSS ~15Mi |
| 高压 | 0ms 嵌入,c=8 | ~6,500 | 3.28 核 | RSS 14.4Mi |
| 饱和 | 0ms 嵌入,c=32 | 7,483 | 3.55 核(封顶) | peak 19.8Mi,p99 12.7ms |

- 内存全水位 ≤20Mi:分配抖动虽大(~150KB/req)但存活对象极小,Go GC 完全压得住
- 容量公式:**CPU 核数 ≈ RPS × 0.5ms**(GOMAXPROCS=4);嵌入延迟主导(≥50ms)时
  单副本吞吐 ≈ 并发/延迟,CPU 需求通常 <0.1 核
- 建议配置:request `cpu 250m / memory 64Mi`;limit `cpu 4`(与 GOMAXPROCS 对齐,
  对实测饱和 3.55 核留 ~13% 余量)/ `memory 256Mi`
- 无 CPU limit 时无 CFS throttling(实测 nr_throttled=0);生产设 limit 后务必
  保持 GOMAXPROCS ≤ limit,否则限流直接抬延迟

## 7. 结论与遗留

- 远程嵌入模式:连接池修复后 CPU/req 降 74%、吞吐 ×2.9,不再是资源大户;剩余
  0.5ms/req 已归因到 JSON 解码/网络/GC(§5.1),无异常热点
- 内置模式:CPU 大头是 rayon 原生推理(§5.3),`RAYON_NUM_THREADS ≈ 常态并发数`
  是延迟↔CPU 的权衡旋钮;进一步优化方向是 embed 层 batch 化(多请求合并推理)与
  intent/classifier 的 ONNX 量化
- keyword 模式 14.5k RPS 下 0.36ms/req,无优化必要
- GOMAXPROCS 超配的代价 = GC 并行放大 + 调度同步(§5.2),生产按 CPU limit 取值
- 后续可选:provider 支持 OpenAI `encoding_format: base64`,压缩嵌入响应解析成本
- pprof 端点(`-pprof`)已合入,生产环境默认关闭,排查时开启
