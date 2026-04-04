# ── Stage 1: Dependencies ──
FROM node:22-alpine AS deps
RUN corepack enable && corepack prepare pnpm@latest --activate
WORKDIR /app
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml* ./
COPY packages/backend/package.json packages/backend/
COPY packages/frontend/package.json packages/frontend/
RUN pnpm install --frozen-lockfile || pnpm install

# ── Stage 2: Build Frontend ──
FROM deps AS build-frontend
WORKDIR /app
COPY packages/frontend/ packages/frontend/
COPY tsconfig.base.json ./
RUN pnpm --filter frontend build

# ── Stage 3: Build Backend ──
FROM deps AS build-backend
WORKDIR /app
COPY packages/backend/ packages/backend/
COPY tsconfig.base.json ./
RUN pnpm --filter backend build

# ── Stage 4: Production Runtime ──
FROM node:22-alpine AS runtime
RUN corepack enable && corepack prepare pnpm@latest --activate
WORKDIR /app

# Copy workspace config
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml* ./
COPY packages/backend/package.json packages/backend/

# Install production deps only
RUN pnpm install --prod --frozen-lockfile || pnpm install --prod

# Copy built artifacts
COPY --from=build-backend /app/packages/backend/dist packages/backend/dist/
COPY --from=build-frontend /app/packages/frontend/dist packages/frontend/dist/

# The backend serves the frontend static files from packages/frontend/dist
ENV NODE_ENV=production
ENV PORT=3000
EXPOSE 3000

CMD ["node", "packages/backend/dist/index.js"]
