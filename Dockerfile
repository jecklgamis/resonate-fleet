FROM ubuntu:24.04 AS build
ARG GO_VERSION=1.26.1
ARG TARGETARCH=amd64
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl && \
    rm -rf /var/lib/apt/lists/* && \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH=/usr/local/go/bin:$PATH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY cmd/resonatectl ./cmd/resonatectl
RUN CGO_ENABLED=0 go build -o /out/resonate-worker . && \
    CGO_ENABLED=0 go build -o /out/resonatectl ./cmd/resonatectl

FROM ubuntu:24.04
LABEL org.opencontainers.image.authors="jecklgamis@gmail.com"

# Which resonate release to bundle, and for which platform - TARGETOS/TARGETARCH
# are set automatically by `docker buildx build` (and by plain `docker build` on
# modern Docker) to match the image being built, so a multi-arch build picks up
# the matching resonate binary without any extra flags.
ARG RESONATE_VERSION=v1.0.0-alpha.2
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates dumb-init curl && \
    rm -rf /var/lib/apt/lists/* && \
    curl -fsSL -o /tmp/resonate.tar.gz \
      "https://github.com/jecklgamis/resonate/releases/download/${RESONATE_VERSION}/resonate_${RESONATE_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz" && \
    tar -xzf /tmp/resonate.tar.gz -C /tmp --strip-components=1 && \
    mv /tmp/resonate /usr/local/bin/resonate && \
    chmod +x /usr/local/bin/resonate && \
    rm -rf /tmp/resonate.tar.gz /tmp/LICENSE /tmp/README.md

EXPOSE 58080

RUN mkdir -p /app/bin /app/workspace
WORKDIR /app

COPY --from=build /out/resonate-worker /out/resonatectl /app/bin/

RUN groupadd --system app && useradd --system --gid app --create-home --home-dir /home/app app
RUN chown -R app:app /app
USER app

ENV WORKSPACE_DIR=/app/workspace
ENV RESONATE_BIN=/usr/local/bin/resonate

ENTRYPOINT ["/usr/bin/dumb-init", "--"]
CMD ["/app/bin/resonate-worker"]
