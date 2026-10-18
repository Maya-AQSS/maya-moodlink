# syntax=docker/dockerfile:1

# --- Compilación ---
# Imagen oficial de Go: no hace falta tener Go instalado en el sistema.
FROM golang:1.25 AS build
WORKDIR /src

# Primero solo go.mod/go.sum: la capa de dependencias se reutiliza mientras no cambien.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# CGO_ENABLED=0: binario estático, necesario para la imagen distroless/static.
# -trimpath y -s -w: sin rutas locales ni símbolos de depuración (binario más pequeño).
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# --- Tests (opcional): docker build --target test . ---
# -race necesita cgo, por eso se ejecuta en la imagen de Go y no en el binario estático.
FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test -race ./...

# --- Producción ---
# distroless/static: sin shell ni curl; solo certificados CA, tzdata y un usuario sin privilegios.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway

USER nonroot:nonroot
EXPOSE 8080

# No hay curl en la imagen: el propio binario consulta /healthz (ver runHealthcheck en main.go).
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/gateway", "-healthcheck"]

ENTRYPOINT ["/gateway"]
