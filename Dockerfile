# ---- build ----
# Usamos la imagen 1.24 para cumplir con el requerimiento de tu go.mod
FROM golang:1.24-alpine as builder

WORKDIR /app

# Instalamos certificados para que el bot pueda hablar con HTTPS (Google/Meta)
RUN apk add --no-cache ca-certificates git

# Cacheamos las dependencias
COPY go.mod go.sum ./
RUN go mod download

# Copiamos el código y compilamos
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/flowly .

# ---- run ----
FROM alpine:3.20
WORKDIR /app

# Certificados necesarios en la imagen final también
RUN apk add --no-cache ca-certificates tzdata

# Copiamos el binario desde la etapa "builder" (Corregido: antes decía "build")
COPY --from=builder /out/flowly /app/flowly

# Copiamos la carpeta de configuraciones y credenciales
COPY configs /app/configs
# IMPORTANTE: Si estás usando google_creds.json, asegurate de que se copie también si está en la raíz
COPY google_creds.json /app/google_creds.json

ENV PORT=8080
EXPOSE 8080

CMD ["/app/flowly"]