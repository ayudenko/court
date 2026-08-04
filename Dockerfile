# Многоступенчатая сборка: бинарники courtd и demo-agent без CGO.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/courtd ./cmd/courtd \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demo-agent ./cmd/demo-agent

FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget su-exec && adduser -D -u 10001 court
COPY --from=build /out/courtd /out/demo-agent /usr/local/bin/
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
WORKDIR /data
EXPOSE 8080
# Стартуем root'ом: entrypoint выдаёт права на /data (volume) и понижает
# привилегии до пользователя court.
ENTRYPOINT ["entrypoint.sh"]
CMD ["courtd"]
