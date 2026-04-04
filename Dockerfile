FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /telefonista .

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=build /telefonista /telefonista
EXPOSE 3000
USER 1000
ENTRYPOINT ["/telefonista"]
