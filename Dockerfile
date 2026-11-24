# The cheesedb server in a container.
#
#   docker build -t cheesedb --build-arg VERSION=v0.4.0 .
#
#   # once: make the admin key (prints the admin token once; keep it)
#   docker run --rm -v cheesedb-data:/data cheesedb serve --admin-key /data/admin.key --init
#
#   # serve, reachable from this machine only
#   docker run -d --name cheesedb -v cheesedb-data:/data -p 127.0.0.1:7700:7700 cheesedb
#
#   # the server made a self-signed certificate; trust it to call the server
#   docker cp cheesedb:/data/tls/tls.crt .
#   curl --cacert tls.crt https://localhost:7700/v1/health
#
# Inside a container the server listens on the container's own address, so
# it serves TLS only: with no certificate given, it makes a self-signed one
# in /data/tls on first start and keeps it. Give your own with --tls-cert
# and --tls-key instead. Everything else it keeps is in /data: the store
# (/data/store), the admin key file (only the admin token's hash) and the
# certificate. One container per store: the store's lock lets only one
# server in.

FROM golang:1.26.5 AS build
WORKDIR /src
COPY . .
ARG VERSION=devel
# Standard library only and no cgo, so the program needs nothing from the
# image it runs in.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.builtVersion=${VERSION}" -o /out/cheesedb ./cmd/cheesedb \
 && mkdir /out/data

# distroless: no shell, no package manager, a non-root user (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cheesedb /usr/local/bin/cheesedb
COPY --from=build --chown=65532:65532 /out/data /data
ENV CHEESEDB_DIR=/data
USER 65532:65532
VOLUME /data
EXPOSE 7700
ENTRYPOINT ["cheesedb"]
CMD ["serve", "--dir", "/data/store", "--listen", "0.0.0.0:7700", "--tls-auto", "/data/tls", "--admin-key", "/data/admin.key"]
