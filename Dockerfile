# yserve — self-hosted Yjs sync server, single static binary.
#
#   docker build -t yserve .
#   docker run -p 8080:8080 -v yserve-data:/data yserve
#
# The image is FROM scratch: ygo is pure Go (no CGO, SQLite included
# via modernc.org/sqlite), so the binary needs no libc and the final
# image is just the binary plus CA roots. cmd/yserve is a module of its
# own; it builds in a workspace with this checkout of the core and of
# persist/sqlite (tools/go-work.sh).

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN ./tools/go-work.sh && go mod download && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /yserve ./cmd/yserve

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /yserve /yserve
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/yserve"]
CMD ["-addr", ":8080", "-store", "/data/ygo.db", "-version-interval", "10m", "-keep-versions", "10"]
