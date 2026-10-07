# syntax=docker/dockerfile:1

# Build on the host's native platform and cross-compile, so multi-arch images
# need no emulation. The app is pure Go (modernc.org/sqlite), hence CGO_ENABLED=0.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/open-pages .
# The final image has no shell, so prepare the data directory here and copy it with
# its ownership; a fresh named volume inherits that ownership.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build /out/open-pages /app/open-pages
# The UI templates are read from ./templates at startup, relative to the working directory.
COPY templates /app/templates
# Container defaults: listen on all interfaces, store everything under /data.
# Mount your own settings.ini over this file to change them.
COPY deploy/settings.ini /etc/open-pages/settings.ini

WORKDIR /app
USER 65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/app/open-pages"]
CMD ["-config", "/etc/open-pages/settings.ini"]
