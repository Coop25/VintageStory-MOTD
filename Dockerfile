FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/motd-changer .

FROM alpine:3.20

RUN adduser -D -H -u 10001 appuser

WORKDIR /app

COPY --from=build /out/motd-changer /app/motd-changer
COPY templates /app/templates

RUN mkdir -p /app/data && chown -R appuser:appuser /app

USER appuser

EXPOSE 8080

ENV MOTD_LISTEN_ADDR=:8080

CMD ["/app/motd-changer"]
