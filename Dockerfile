# Multi-stage build: parser binary + parse-service HTTP wrapper.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/parser . \
 && CGO_ENABLED=0 go build -o /out/server ./cmd/server

FROM alpine:3.20
RUN adduser -D app
USER app
WORKDIR /app
COPY --from=build /out/parser /out/server ./
ENV PARSER_BIN=/app/parser PORT=8080
EXPOSE 8080
CMD ["./server"]
