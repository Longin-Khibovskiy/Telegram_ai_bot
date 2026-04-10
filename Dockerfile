FROM golang:1.26.2-alpine

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

CMD ["go", "run", "."]