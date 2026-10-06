# The Go binary is cross-compiled on the build machine's own architecture, so
# multi-arch images don't compile under emulation.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /revq ./cmd/revq

FROM node:22-slim
# node:22-slim ships no CA bundle, and the static Go binary reads its roots from
# /etc/ssl/certs, so without this every api.github.com call fails to verify.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g @anthropic-ai/claude-code && npm cache clean --force \
    && mkdir /data && chown node:node /data
COPY --from=build /revq /usr/local/bin/revq
USER node
ENV REVQ_DB=/data/revq.db REVQ_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["revq"]
