# One Dockerfile for all services: the SERVICE argument specifies which one to build.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Copy dependencies first; this layer is cached until go.mod/go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG SERVICE
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

FROM alpine:3.20
RUN adduser -D -u 10001 app
COPY --from=build /out/app /app
USER app
ENTRYPOINT ["/app"]
