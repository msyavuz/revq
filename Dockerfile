FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /revq ./cmd/revq

FROM node:22-slim
RUN npm install -g @anthropic-ai/claude-code && npm cache clean --force \
    && mkdir /data && chown node:node /data
COPY --from=build /revq /usr/local/bin/revq
USER node
ENV REVQ_DB=/data/revq.db REVQ_ADDR=:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["revq"]
