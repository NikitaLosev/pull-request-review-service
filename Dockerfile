# Stage 1: Сборка бинарного файла
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Копируем и скачиваем зависимости
COPY go.mod go.sum ./
RUN go mod download

# Копируем исходный код и собираем приложение
COPY . .
# CGO_ENABLED=0 для создания статически слинкованного бинарника
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/server ./cmd/api/main.go

# Stage 2: Финальный образ
FROM alpine:latest

# Добавляем CA сертификаты
RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Копируем бинарный файл из стадии сборки
COPY --from=builder /bin/server .

EXPOSE 8080

# Запускаем сервер
CMD ["./server"]
