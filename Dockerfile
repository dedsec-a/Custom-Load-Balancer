# One image for every service: the load balancer, the simulated backend and
# the built dashboard. docker-compose.yml picks which program each container runs.

# ---- 1. build the dashboard ------------------------------------------------
FROM node:22-alpine AS ui
WORKDIR /ui
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

# ---- 2. build the Go programs ----------------------------------------------
FROM golang:1.27-alpine AS go
WORKDIR /src
COPY backend/go.mod backend/go.sum* ./
RUN go mod download
COPY backend/ ./
RUN go test ./internal/... \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/lb ./cmd/lb \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backend ./cmd/backend

# ---- 3. small runtime image -----------------------------------------------
FROM alpine:3.22
# BusyBox wget (already in Alpine) is enough for the Compose health checks.
RUN adduser -D -H -u 10001 app \
 && mkdir -p /data/experiments && chown app /data/experiments   # a new named volume copies this owner
COPY --from=go /out/ /usr/local/bin/
COPY --from=ui /ui/dist /app/ui
USER app
