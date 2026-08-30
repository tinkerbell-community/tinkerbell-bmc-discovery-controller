# Built by goreleaser (dockers_v2): the build context contains the
# cross-compiled binary at <goos>/<goarch>/manager plus extra_files.
# Use `make snapshot` for a local build; plain `docker build` won't work.
FROM --platform=$BUILDPLATFORM alpine:latest AS ca-certs

RUN apk --no-cache add ca-certificates

FROM scratch
ARG TARGETOS
ARG TARGETARCH

COPY ${TARGETOS}/${TARGETARCH}/manager /manager
COPY --from=ca-certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY LICENSE /usr/share/doc/tinkerbell-bmc-discovery-controller/LICENSE

ENTRYPOINT ["/manager"]
