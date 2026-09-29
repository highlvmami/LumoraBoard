# One image with the Go server and the built frontend, served from the
# same origin so sign-in cookies and the WebSocket need no CORS.

FROM node:22-alpine AS web
WORKDIR /src
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS server
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /lumora ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /lumora /lumora
COPY --from=web /src/build /web
ENV LUMORA_ADDR=:8080 LUMORA_STATIC_DIR=/web
EXPOSE 8080
ENTRYPOINT ["/lumora"]
