FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /out/resonate-worker .

FROM alpine:3.20
LABEL org.opencontainers.image.authors="jecklgamis@gmail.com"

# Which resonate release to bundle, and for which platform - TARGETOS/TARGETARCH
# are set automatically by `docker buildx build` (and by plain `docker build` on
# modern Docker) to match the image being built, so a multi-arch build picks up
# the matching resonate binary without any extra flags.
ARG RESONATE_VERSION=v1.0.0-alpha.2
ARG TARGETOS=linux
ARG TARGETARCH=amd64

RUN apk add --no-cache ca-certificates dumb-init curl && \
    curl -fsSL -o /tmp/resonate.tar.gz \
      "https://github.com/jecklgamis/resonate/releases/download/${RESONATE_VERSION}/resonate_${RESONATE_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz" && \
    tar -xzf /tmp/resonate.tar.gz -C /tmp --strip-components=1 && \
    mv /tmp/resonate /usr/local/bin/resonate && \
    chmod +x /usr/local/bin/resonate && \
    rm -rf /tmp/resonate.tar.gz /tmp/LICENSE /tmp/README.md /var/cache/apk/*

EXPOSE 58080

RUN mkdir -p /app/bin /app/workspace
WORKDIR /app

COPY --from=build /out/resonate-worker /app/bin/resonate-worker

RUN addgroup -S app && adduser -S app -G app -h /home/app
RUN chown -R app:app /app
USER app

ENV WORKSPACE_DIR=/app/workspace
ENV RESONATE_BIN=/usr/local/bin/resonate

ENTRYPOINT ["/usr/bin/dumb-init", "--"]
CMD ["/app/bin/resonate-worker"]
