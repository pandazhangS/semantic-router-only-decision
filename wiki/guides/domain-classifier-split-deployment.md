# 分离式 mmBERT domain 识别服务部署指南(CPU/GPU 异构)

> 2026-09-02。对应实现:`category_model.protocol: http_classify`(路由侧)+
> `cmd/domain-classifier`(独立服务),E2E 已在 Ubuntu 实测通过。
> 方案背景见 [domain-to-kb-signal-migration.md](../research/domain-to-kb-signal-migration.md) §4.1。

## 1. 拓扑与收益

```
┌─ 决策面 decision-server(CPU 副本,可 HPA by RPS)─────┐
│  keyword/structure/complexity/KB 信号 + domain 走远程  │
└──────────────┬───────────────────────────────────────┘
               │ POST /classify (http_classify 契约)
┌──────────────▼───────────────────────────────────────┐
│ 分类面 domain-classifier(mmBERT 权重独立驻留)         │
│  CPU 独占核或 GPU 节点,独立 HPA/独立 SLO              │
└──────────────────────────────────────────────────────┘
```

- 决策面不再为 7~12ms/req 的本地 BERT 前向买单;镜像可进一步去掉模型与 cgo 依赖
- 分类面按自己的水位扩缩(GPU 或独占 CPU),不再与决策面互抢资源
- 与 KB 方案互补:KB=零新模型;外置=保留训练好的 domain 分类头

## 2. 前置:镜像与模型

### 2.1 构建镜像(Dockerfile 双 target)

```bash
# 独立分类服务(复用 Rust 编译缓存层)
docker build --target domain-classifier -t domain-classifier:latest .

# 路由决策面(现有默认 target,不变)
docker build -t decision-server:latest .
```

### 2.2 模型资产(≈1.23GB,6 个文件)

```bash
mkdir -p /tmp/domain-model && cd /tmp/domain-model
BASE=https://hf-mirror.com/llm-semantic-router/mmbert32k-intent-classifier-merged/resolve/main
for f in config.json model.safetensors tokenizer.json tokenizer_config.json special_tokens_map.json category_mapping.json; do
  curl -fL --retry 3 -o "$f" "$BASE/$f"
done
```

> 模型是标准 `ModernBertForSequenceClassification`,`InitModernBertClassifier` 直接加载
> (实测启动 ~2s);`category_mapping.json` 必须与服务加载的是同一份(标签按名对齐)。

## 3. Helm 部署

```bash
# 建议生产把 values 中 model.hostPath 换成 PVC
helm install domain-split deploy/helm/decision-server-domain-split \
  --set decisionServer.embedding.apiKey="$EMBEDDING_API_KEY"
```

chart:`deploy/helm/decision-server-domain-split/`
- `domain-classifier` Deployment/Service:`/healthz` 探针、模型 hostPath(生产换 PVC)、
  nodeSelector/tolerations 可指定 GPU 或独占 CPU 节点;资源建议 request 1C/2Gi
- `decision-server` Deployment/Service + ConfigMap:配置即
  `config/decision-server.yaml` + 分离模式增量(已通过 canonical loader 校验):
  - `global.model_catalog.modules.classifier.domain.protocol: http_classify`
  - `global.model_catalog.external[]` 增加 `model_role: classification` 条目
  - `routing.signals.domains`(mmlu_categories 分组)+ 引用 domain 信号的 decisions

关键配置片段(完整见 chart 模板):

```yaml
global:
  model_catalog:
    external:
      - name: domain-classifier-svc
        model_role: classification
        llm_endpoint: {address: domain-classifier-svc, port: 8090, protocol: http}
    modules:
      classifier:
        domain:
          protocol: http_classify
          category_mapping_path: /models/domain/category_mapping.json
```

## 4. 验证

```bash
# 服务直连
curl -X POST http://<domain-classifier>:8090/classify \
  -H 'Content-Type: application/json' \
  -d '{"inputs":"What is the derivative of x squared with respect to x?"}'
# → [{"label":"math","score":0.795},{"label":"physics","score":0.191},...14 项]

# 路由侧集成测试(真实构造路径 + evaluateDomainSignal;平时跳过,设 env 才跑)
DOMAIN_CLASSIFIER_E2E_URL=http://127.0.0.1:18090 \
DOMAIN_CLASSIFIER_E2E_MAPPING=/models/domain/category_mapping.json \
  go test -run TestDomainExternalService_EndToEnd ./pkg/classification/
```

实测记录(2026-09-02,Ubuntu 24.04,共享 CPU 测试机):模型加载 2s;math 样例
confidence 0.7945 与直连完全一致;单次 classify ~140-180ms(**非生产口径**,生产
GPU/独占核部署后按 [压测规范](../research/decision-server-optimization.md)(70ms
真实延迟 mock,禁用 0ms mock)另行压测)。

## 5. 运维要点

- **失败语义**:分类服务不可用时 domain 信号报错跳过(fail-open),不阻塞其它信号;
  需要严格语义时在决策层组合其它信号兜底
- **扩缩维度**:分类面按 QPS 与单请求时延扩;决策面按 RPS 线性(`CPU ≈ RPS × 0.4ms`)
- **模型更新**:权重目录热替换 + 重启 pod 即可,路由侧只依赖 mapping 文件名对齐
- **mapping 一致性**:路由侧与服务侧的 `category_mapping.json` 必须同源(建议纳入
  同一镜像/ConfigMap 版本化流程)

## 6. 约束

- 概率版 FFI 仅 ModernBERT/Candle-BERT 变体可用;mmBERT-32K 专用 intent FFI 无
  WithProbs——外置服务要求模型为 ModernBERT 格式(merged intent 模型实测可用)
- 响应必须覆盖 mapping 全部标签且分数和 ≈1.0(`alignScoresToMapping` 严格校验)
- 标签对齐按名字而非顺序,两侧 mapping 版本不一致会在启动/请求期显式报错
