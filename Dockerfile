FROM node:22-bookworm-slim AS client-build
WORKDIR /src/client
COPY client/package*.json ./
RUN npm ci
COPY client/ ./
RUN npm run build

FROM golang:1.26-bookworm AS server-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY --from=client-build /src/client/dist ./client/dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/wacalls ./cmd/server

FROM debian:bookworm-slim
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=server-build /out/wacalls /usr/local/bin/wacalls
COPY --from=client-build /src/client/dist /app/client/dist
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/wacalls"]
CMD ["-addr", ":8080", "-db", "/data/wacalls.db", "-static", "/app/client/dist"]
