# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src
# No external dependencies — copy everything and build a static binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /clip .

# ---- runtime stage ----
FROM alpine:3.20
# Pin UID/GID so a pre-existing named volume stays writable across rebuilds.
RUN addgroup -g 10001 -S clip && adduser -u 10001 -S clip -G clip \
    && mkdir -p /data && chown clip:clip /data
COPY --from=build /clip /usr/local/bin/clip

USER clip
ENV CLIP_DATA=/data PORT=8080
EXPOSE 8080
VOLUME /data

HEALTHCHECK --interval=30s --timeout=4s --start-period=5s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["clip"]
