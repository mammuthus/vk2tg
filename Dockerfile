FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/vk2tg .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 10001 vk2tg \
    && adduser -S -D -H -u 10001 -G vk2tg vk2tg \
    && mkdir /data \
    && chown 10001:10001 /data \
    && chmod 700 /data
COPY --from=build /out/vk2tg /usr/local/bin/vk2tg
USER 10001:10001
WORKDIR /data
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/vk2tg"]