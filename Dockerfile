FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /telefonista .

FROM alpine:3
RUN apk add --no-cache ca-certificates
COPY --from=build /telefonista /telefonista
ENTRYPOINT ["/telefonista"]
