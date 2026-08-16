FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.Version=${VERSION}" -o /silo ./cmd/silo

# :nonroot runs as uid/gid 65532 instead of root. A bind-mounted data
# directory must be readable and writable by that uid:
#   chown -R 65532:65532 /path/to/silo-data
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /silo /usr/local/bin/silo
ENV SILO_DATA_DIR=/data
# The binary defaults to loopback, which inside a container means the
# published port reaches nothing. A container's network namespace is the
# isolation boundary, so binding all of its interfaces is the right default
# here even though it is not the right default for the binary.
ENV SILO_HOST=0.0.0.0
VOLUME /data
EXPOSE 8082
USER 65532:65532
ENTRYPOINT ["silo", "serve"]
