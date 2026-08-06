FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/plexamp-mosh-api ./cmd/webapi

FROM alpine:3.21

RUN apk add --no-cache ca-certificates ffmpeg \
    && addgroup -S plexamp \
    && adduser -S -G plexamp plexamp \
    && mkdir -p /data \
    && chown plexamp:plexamp /data

COPY --from=build /out/plexamp-mosh-api /usr/local/bin/plexamp-mosh-api

USER plexamp
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/plexamp-mosh-api"]
