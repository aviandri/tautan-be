# syntax=docker/dockerfile:1
# Multi-arch (amd64/arm64): Go cross-compiles on the build host, no QEMU.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && mkdir -p /out/data/uploads

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
# Writable by the nonroot user (65532); a named volume mounted here inherits it.
COPY --from=build --chown=65532:65532 /out/data /data
ENV ADDR=:8080 UPLOAD_DIR=/data/uploads APP_ENV=production
USER 65532:65532
EXPOSE 8080
VOLUME ["/data/uploads"]
# `api healthcheck` GETs /livez (the image has no shell or curl).
HEALTHCHECK --interval=15s --timeout=4s --start-period=20s --retries=3 CMD ["/api", "healthcheck"]
ENTRYPOINT ["/api"]
