FROM golang:1.23.4-alpine AS go-builder

RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev sqlite-dev

WORKDIR /app/server

COPY siege-ct-server/ .

RUN go mod tidy && go mod download

RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o main .

FROM node:18-alpine AS node-builder

WORKDIR /app

COPY package*.json ./

RUN npm ci

COPY src/ ./src/
COPY public/ ./public/
COPY next.config.ts ./
COPY tsconfig.json ./
COPY postcss.config.mjs ./
COPY eslint.config.mjs ./

RUN npm run build
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata wget
WORKDIR /app

COPY --from=go-builder /app/server/main .
COPY --from=go-builder /app/server/.env .
COPY --from=node-builder /app/out/ ./public/
COPY --from=node-builder /app/.next/static ./public/_next/static/
COPY --from=node-builder /app/public/* ./public/
RUN mkdir -p /app/data
ENV PORT=3421
ENV NODE_ENV=production
EXPOSE 3421
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:3421/health || exit 1
CMD ["./main"]