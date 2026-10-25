# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.10 AS build
WORKDIR /src
ARG APP_VERSION=dev
ARG TARGETOS
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
  go build -trimpath -ldflags "-s -w -X main.version=${APP_VERSION}" -o /out/y6ifttt . \
  && mkdir -p /out/data

# Static, CGO-free binary (pure-Go SQLite) on distroless: no shell, no package
# manager, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
ARG APP_VERSION=dev
LABEL org.opencontainers.image.title="yoursix-ifttt" \
      org.opencontainers.image.description="Programmable actions for YourSix webhook events" \
      org.opencontainers.image.version="${APP_VERSION}"
WORKDIR /app
COPY --from=build /out/y6ifttt /usr/local/bin/y6ifttt
# Owned by nonroot so a fresh named volume mounted here is writable.
COPY --from=build --chown=65532:65532 /out/data /app/data
ENV Y6IFTTT_DATA_DIR=/app/data
EXPOSE 8080
VOLUME ["/app/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/y6ifttt", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/y6ifttt"]
CMD ["serve"]
