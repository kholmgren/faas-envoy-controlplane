FROM golang:1.15.6-alpine3.12 AS builder
WORKDIR /app
ADD go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o ./bin/faas-envoy-controlplane .

FROM alpine:3.12.2
WORKDIR /app
COPY --from=builder /app/bin/faas-envoy-controlplane ./
ENTRYPOINT [ "./faas-envoy-controlplane" ]

