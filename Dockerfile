# Build em duas etapas: a imagem final não carrega o toolchain do Go.
FROM golang:1.27-alpine AS build

WORKDIR /src

# As dependências entram antes do código, para que a camada de módulos só
# seja refeita quando go.mod ou go.sum mudarem.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO desligado: binário estático, que roda na imagem mínima sem libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wallet ./cmd/wallet

FROM alpine:3.21

# Certificados raiz: o cliente OIDC fala HTTPS com o IdP.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 wallet

COPY --from=build /out/wallet /usr/local/bin/wallet

# Usuário sem privilégio: o processo não precisa de root para nada.
USER wallet
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/wallet"]
