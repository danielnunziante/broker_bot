# ---- build ----
FROM golang:1.22-alpine AS build
WORKDIR /app
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/flowly ./.

# ---- run ----
FROM alpine:3.20
WORKDIR /app
RUN apk add --no-cache ca-certificates

COPY --from=build /out/flowly /app/flowly

# Copiamos configs al contenedor
COPY configs /app/configs

ENV PORT=8080
EXPOSE 8080

CMD ["/app/flowly"]