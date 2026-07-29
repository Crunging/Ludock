# syntax=docker/dockerfile:1

ARG ALPINE_VERSION=3.24

# Build JavaScript artifacts on the native build platform.
FROM --platform=$BUILDPLATFORM node:24-alpine${ALPINE_VERSION} AS deps
RUN corepack enable && corepack prepare pnpm@10.28.2 --activate
WORKDIR /app
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml* ./
COPY packages/backend/package.json packages/backend/
COPY packages/frontend/package.json packages/frontend/
RUN pnpm install --frozen-lockfile

FROM --platform=$BUILDPLATFORM deps AS build-frontend
WORKDIR /app
COPY packages/frontend/ packages/frontend/
COPY tsconfig.base.json ./
RUN pnpm --filter @ludock/frontend build

FROM --platform=$BUILDPLATFORM deps AS build-backend
WORKDIR /app
COPY packages/backend/ packages/backend/
COPY tsconfig.base.json ./
RUN pnpm --filter @ludock/backend build

FROM --platform=$BUILDPLATFORM node:24-alpine${ALPINE_VERSION} AS prod-deps
RUN corepack enable && corepack prepare pnpm@10.28.2 --activate
WORKDIR /app
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml* ./
COPY packages/backend/package.json packages/backend/
RUN pnpm install --prod --frozen-lockfile

# The runtime executable must match the target platform.
FROM node:24-alpine${ALPINE_VERSION} AS node-runtime

FROM alpine:${ALPINE_VERSION} AS runtime
RUN apk add --no-cache libstdc++
WORKDIR /app

COPY --from=node-runtime /usr/local/bin/node /usr/local/bin/node
COPY packages/backend/package.json packages/backend/

# pnpm's package links depend on both node_modules directories.
COPY --from=prod-deps /app/node_modules node_modules/
COPY --from=prod-deps /app/packages/backend/node_modules packages/backend/node_modules/

COPY --from=build-backend /app/packages/backend/dist packages/backend/dist/
COPY --from=build-frontend /app/packages/frontend/dist packages/frontend/dist/

ENV NODE_ENV=production
ENV PORT=3000
ENV LUDOCK_DB_PATH=/data/ludock.db
VOLUME ["/data"]
EXPOSE 3000
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://localhost:3000/api/health || exit 1

CMD ["node", "packages/backend/dist/index.js"]
