FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod download && CGO_ENABLED=0 go build -o server .

FROM alpine:latest
COPY --from=builder /app/server .
EXPOSE 8080
CMD ["./server"]