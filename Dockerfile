FROM golang:1.21-alpine AS builder

WORKDIR /app

# Instala dependências do sistema (necessário para sqlite cgo)
RUN apk add --no-cache gcc musl-dev

# 1. Copia TODO o projeto primeiro (incluindo main.go)
COPY . .

# 2. Agora roda o tidy. Como ele vê o main.go, ele vai baixar o sqlite3 e gerar o go.sum corretamente
RUN go mod tidy

# 3. Build
RUN CGO_ENABLED=1 GOOS=linux go build -o main .

# --- Estágio Final ---
FROM alpine:latest
WORKDIR /app

COPY --from=builder /app/main .
COPY --from=builder /app/templates ./templates

# Cria diretório de dados
RUN mkdir -p /app/data

EXPOSE 8086
CMD ["./main"]