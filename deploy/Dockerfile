FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/toll ./cmd/toll
RUN mkdir -p /out/home/data /out/home/events

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/toll /toll
# Relative paths in the config (./events, ./data) resolve here. The
# directories are created owned by nonroot so that volumes mounted on them
# are writable; mount volumes there to keep them across restarts.
COPY --from=build --chown=65532:65532 /out/home/ /home/nonroot/
WORKDIR /home/nonroot
# 8655: proxy, 8656: admin (dashboard, metrics, health)
EXPOSE 8655 8656
ENTRYPOINT ["/toll"]
