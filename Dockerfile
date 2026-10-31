# syntax=docker/dockerfile:1

# Build on the host's native platform and cross-compile, so multi-arch images
# need no emulation. The app is pure Go (modernc.org/sqlite), hence CGO_ENABLED=0.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/open-pages ./cmd/open-pages
# The final image has no shell, so prepare the data directory here and copy it with
# its ownership and mode; a fresh named volume inherits both. The directories are copied
# as children of /out/fs, because COPY of a directory copies its contents, and BuildKit
# versions differ on whether --chmod also applies to the destination directory itself. Group 0 owns /data with
# group permissions equal to the owner's (g=u) and setgid, so any UID that runs with
# GID 0 can write it: OpenShift's restricted-v2 SCC assigns a random UID and GID 0.
# /tmp (sticky, world-writable) is a mount point for an optional tmpfs/emptyDir.
RUN mkdir -p /out/fs/data /out/fs/tmp \
 && chown 65532:0 /out/fs/data && chmod 2775 /out/fs/data \
 && chmod 1777 /out/fs/tmp

FROM scratch
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="open-pages" \
      org.opencontainers.image.description="Self-hosted static site hosting with per-site access control" \
      org.opencontainers.image.source="https://github.com/gira0/open-pages" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/fs/ /
COPY --from=build /out/open-pages /app/open-pages
# The license, and notices for the third-party CSS and fonts embedded in the binary.
COPY LICENSE NOTICE web/static/patternfly/*.txt /usr/share/doc/open-pages/
# Container defaults: listen on all interfaces, store everything under /data.
# Mount your own settings.ini over this file to change them.
COPY deploy/settings.ini /etc/open-pages/settings.ini

WORKDIR /app
# A numeric user and group, so Kubernetes' runAsNonRoot check passes without an /etc/passwd
# lookup (there is none). Platforms that assign their own UID (OpenShift) keep GID 0, which
# owns /data. There is no HEALTHCHECK: the image has no shell or curl and the binary has no
# probe subcommand, so probe GET /readyz and /livez from the orchestrator instead.
USER 65532:0
# Go's multipart uploads spill to TMPDIR. The root may be read-only and /tmp is then not
# writable, so default to the volume (the app creates /data/tmp from tmppath at startup).
# With a writable /tmp (tmpfs or emptyDir) you can set TMPDIR=/tmp to keep spooled uploads
# off the volume.
ENV TMPDIR=/data/tmp
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/app/open-pages"]
CMD ["-config", "/etc/open-pages/settings.ini"]
