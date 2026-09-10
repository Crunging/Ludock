ARG ALPINE_IMAGE=alpine:3@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
ARG BUN_IMAGE=oven/bun:1-alpine@sha256:d888c0ae6c86d7866ff10c5aafdd9077b36aee6455b33dd270fb93c0dd5cef6f

# Build JavaScript artifacts on the native build platform.
FROM --platform=$BUILDPLATFORM ${BUN_IMAGE} AS deps
WORKDIR /app
COPY package.json bun.lock bunfig.toml ./
COPY packages/backend/package.json packages/backend/
COPY packages/frontend/package.json packages/frontend/
COPY packages/shared/package.json packages/shared/
RUN bun install --frozen-lockfile --linker=isolated

FROM --platform=$BUILDPLATFORM deps AS build-frontend
WORKDIR /app
COPY packages/frontend/ packages/frontend/
COPY packages/shared/ packages/shared/
COPY tsconfig.base.json ./
RUN bun run --filter @ludock/shared build && bun run --filter @ludock/frontend build

FROM --platform=$BUILDPLATFORM deps AS build-backend
WORKDIR /app
COPY packages/backend/ packages/backend/
COPY packages/shared/ packages/shared/
COPY tsconfig.base.json ./
RUN bun run --filter @ludock/shared build && bun run --filter @ludock/backend build

FROM --platform=$BUILDPLATFORM ${BUN_IMAGE} AS prod-deps
WORKDIR /app
COPY package.json bun.lock bunfig.toml ./
COPY packages/backend/package.json packages/backend/
COPY packages/frontend/package.json packages/frontend/
COPY packages/shared/package.json packages/shared/
RUN bun install --production --frozen-lockfile --linker=isolated \
    --filter @ludock/backend --filter @ludock/shared

# The runtime executable must match the target platform.
FROM ${BUN_IMAGE} AS bun-runtime

FROM ${ALPINE_IMAGE} AS runtime
RUN apk upgrade --no-cache \
    && apk add --no-cache libstdc++ docker-cli docker-cli-compose
WORKDIR /app

COPY --from=bun-runtime /usr/local/bin/bun /usr/local/bin/bun
COPY packages/backend/package.json packages/backend/
COPY packages/shared/package.json packages/shared/

# Bun's isolated package links depend on the store and workspace directories.
COPY --from=prod-deps /app/node_modules node_modules/
COPY --from=prod-deps /app/packages/backend/node_modules packages/backend/node_modules/
COPY --from=prod-deps /app/packages/shared/node_modules packages/shared/node_modules/

COPY --from=build-backend /app/packages/backend/dist packages/backend/dist/
COPY --from=build-backend /app/packages/shared/dist packages/shared/dist/
COPY --from=build-frontend /app/packages/frontend/dist packages/frontend/dist/

ENV NODE_ENV=production
ENV PORT=3000
ENV LUDOCK_DB_PATH=/data/ludock.db
VOLUME ["/data"]
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost:3000/api/v1/health || exit 1

CMD ["bun", "packages/backend/dist/index.js"]
