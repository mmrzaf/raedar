# The build stage needs only the Go toolchain image. The runtime image is
# scratch: the binary is static and the CA bundle is copied in.
#
#   docker build \
#     --build-arg GO_IMAGE=registry.example.com/library/golang:1.27-alpine \
#     --build-arg GOPROXY=https://goproxy.example.com,direct \
#     --build-arg VERSION=0.1.0 \
#     -t raedar:0.1.0 .
ARG GO_IMAGE=golang:1.27-alpine

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} \
    GOTOOLCHAIN=local \
    CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/raedar ./cmd/raedar

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/raedar /usr/local/bin/raedar
ENV PATH=/usr/local/bin:/usr/bin:/bin
EXPOSE 8080
# nobody. The system D-Bus only lets users that exist in the host's user
# database connect, and every Linux host has this one.
USER 65534:65534
HEALTHCHECK --interval=30s --timeout=5s --retries=3 CMD ["/usr/local/bin/raedar", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/raedar"]
CMD ["serve"]
