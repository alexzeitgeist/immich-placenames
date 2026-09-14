# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26.8-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81 AS build

ENV GOTOOLCHAIN=local

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG VCS_REF

RUN CGO_ENABLED=0 \
    GOOS="${TARGETOS}" \
    GOARCH="${TARGETARCH}" \
    go build \
      -mod=readonly \
      -trimpath \
      -buildvcs=false \
      -ldflags="-s -w -X main.buildRevision=${VCS_REF}" \
      -o /out/immich-placenames \
      ./cmd/immich-placenames


FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

ARG VCS_REF=unknown
ARG VERSION=dev

LABEL org.opencontainers.image.title="immich-placenames" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}"

RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir /data \
    && chown 10001:10001 /data

COPY --from=build /out/immich-placenames /usr/local/bin/immich-placenames
COPY LICENSE NOTICE /licenses/

WORKDIR /
USER 10001:10001

ENTRYPOINT ["/usr/local/bin/immich-placenames"]
