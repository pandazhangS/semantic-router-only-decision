# syntax=docker/dockerfile:1
# decision-server 镜像（多阶段构建，缓存友好）
#
#   stage 1: Rust 编译 candle-binding / nlp-binding（CPU 模式，--no-default-features 禁用 CUDA）
#   stage 2: Go 编译 decision-server（cgo 链接 stage 1 的 .so）
#   stage 3: 运行时（仅二进制 + .so + 示例配置）
#
# 缓存策略：manifest/lock 与源码分层 COPY，依赖层不变时命中缓存，避免重复拉取/编译依赖。
#
# 构建：docker build -t decision-server:latest .
# 运行：docker run -p 8080:8080 -e EMBEDDING_API_KEY=xxx decision-server:latest

# ---------- Stage 1: Rust bindings ----------
FROM rust:1.98-bookworm AS bindings
ENV RUSTUP_DIST_SERVER=https://rsproxy.cn \
    RUSTUP_UPDATE_ROOT=https://rsproxy.cn/rustup \
    CARGO_REGISTRIES_CRATES_IO_PROTOCOL=sparse \
    CARGO_REGISTRIES_CRATES_IO_INDEX=sparse+https://rsproxy.cn/index/ \
    CARGO_NET_GIT_FETCH_WITH_CLI=true
# github 直连不稳定，git 依赖走 ghfast.top 代理
RUN git config --global url."https://ghfast.top/https://github.com/".insteadOf "https://github.com/"
WORKDIR /build
# 依赖层：Cargo.toml/Cargo.lock 不变则命中缓存
COPY candle-binding/Cargo.toml candle-binding/Cargo.lock ./candle-binding/
COPY nlp-binding/Cargo.toml nlp-binding/Cargo.lock ./nlp-binding/
# cargo fetch 需要 src/lib.rs 存在（占位，随后被真实源码覆盖）
RUN mkdir -p candle-binding/src nlp-binding/src \
 && touch candle-binding/src/lib.rs nlp-binding/src/lib.rs \
 && cargo fetch --manifest-path candle-binding/Cargo.toml \
 && cargo fetch --manifest-path nlp-binding/Cargo.toml
# 源码层
COPY candle-binding/ ./candle-binding/
COPY nlp-binding/ ./nlp-binding/
RUN cargo build --release --no-default-features --manifest-path candle-binding/Cargo.toml
RUN cargo build --release --manifest-path nlp-binding/Cargo.toml

# ---------- Stage 2: Go 编译 ----------
FROM golang:1.27.0-bookworm AS build
ENV GOPROXY=https://goproxy.cn,direct \
    CGO_ENABLED=1
WORKDIR /src
# 依赖层：go.mod/go.sum 不变则命中缓存
COPY src/semantic-router/go.mod src/semantic-router/go.sum ./src/semantic-router/
COPY candle-binding/go.mod ./candle-binding/
COPY nlp-binding/go.mod ./nlp-binding/
RUN cd src/semantic-router && go mod download
# 源码层
COPY candle-binding/ ./candle-binding/
COPY nlp-binding/ ./nlp-binding/
COPY --from=bindings /build/candle-binding/target/release/ ./candle-binding/target/release/
COPY --from=bindings /build/nlp-binding/target/release/ ./nlp-binding/target/release/
COPY src/semantic-router/ ./src/semantic-router/
RUN cd src/semantic-router && go build -o /out/decision-server ./cmd/decision-server

# ---------- Stage 3: 运行时 ----------
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends libgcc-s1 ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/decision-server /usr/local/bin/decision-server
COPY --from=bindings /build/candle-binding/target/release/libcandle_semantic_router.so /usr/local/lib/
COPY --from=bindings /build/nlp-binding/target/release/libnlp_binding.so /usr/local/lib/
COPY config/decision-server.yaml /etc/decision-server/config.yaml
ENV LD_LIBRARY_PATH=/usr/local/lib
EXPOSE 8080
ENTRYPOINT ["decision-server"]
CMD ["-config", "/etc/decision-server/config.yaml", "-listen", ":8080"]