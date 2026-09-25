# syntax=docker/dockerfile:1

# Build: mesma versão de Go declarada no go.mod.
FROM golang:1.27.1 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# Runtime: imagem mínima, sem shell, usuário não root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/migrate /app/
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/api"]
