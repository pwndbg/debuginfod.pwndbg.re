FROM golang:1.26-alpine AS backend-build

# erofs-utils is a RUNTIME dependency, not a build one: nix.NewNixDebuginfo
# log.Fatal's on startup if mkfs.erofs is not in $PATH. This image is single
# stage, so installing it here also puts it in the running container.
#
# Only lz4 compression is usable - see TarballToErofs in nix/util.go, which
# explains why deflate builds fine here and then cannot be mounted on the host.
RUN apk add --no-cache ca-certificates tzdata erofs-utils

WORKDIR /app/
COPY ./go.mod ./go.sum ./
RUN go mod download
COPY cmd/ ./cmd
COPY useragent/ ./useragent
# The nix/ package, which cmd/nix-debuginfod imports. The other two Dockerfiles copy
# cmd/ alone because nothing they build needs it; this one does.
COPY nix/ ./nix
# cmd/nix-debuginfod shares its source-path matcher with cmd/deb-debuginfod.
COPY srcindex/ ./srcindex

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/nix.bin ./cmd/nix-debuginfod/

EXPOSE 8034
ENV TZ=Europe/Warsaw

CMD ["./nix.bin"]
