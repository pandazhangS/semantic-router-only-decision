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

## 5. 结论与遗留

- 远程嵌入模式:连接池修复后 CPU/req 降 74%、吞吐 ×2.9,不再是资源大户
- 内置模式:CPU 大头是模型推理本身,`RAYON_NUM_THREADS` 提供延迟↔CPU 权衡;进一步优化方向是 embed 层 batch 化(多请求合并推理)与 intent/classifier 的 ONNX 量化
- keyword 模式 14.5k RPS 下 0.36ms/req,无优化必要
- pprof 端点(`-pprof`)已合入,生产环境默认关闭,排查时开启
