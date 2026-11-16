# Multi-stage build for any binary under cmd/. Usage:
#   docker build --build-arg BIN=tollgate-gw -t tollgate-gw .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG BIN
RUN test -n "$BIN" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${BIN}
# A writable /data owned by nonroot: Docker copies this ownership into a
# fresh named volume mounted there (tollgate-control keeps its Raft WAL in
# /data; without this the volume is root-owned and the node can't start).
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
COPY --from=build --chown=65532:65532 /out/data /data
USER nonroot:nonroot
ENTRYPOINT ["/app"]
