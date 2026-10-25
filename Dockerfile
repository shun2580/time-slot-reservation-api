# 多段ビルド。builder は Go 1.23.4 に固定、実行側は軽い非 root の alpine。
FROM golang:1.23.4-alpine3.21 AS builder

ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0

WORKDIR /src

# 依存の取得を先に行い、ソースの変更でキャッシュが無効にならぬようにする。
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM alpine:3.21.0

# 非 root で動かす。tzdata は main が time/tzdata を取り込むので入れない。
RUN addgroup -S -g 10001 app && adduser -S -u 10001 -G app app

COPY --from=builder /out/api /usr/local/bin/api

USER app
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/api"]
