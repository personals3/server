# =============================================================================
# Multi-stage Next.js build using the "standalone" output mode
# (next.config.mjs sets output: "standalone" → tiny runtime image)
# =============================================================================

FROM node:22-alpine AS deps
WORKDIR /app
COPY package.json ./
RUN npm install --no-audit --no-fund

# ---- builder
FROM node:22-alpine AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
# Build-time public env vars — Next.js bakes NEXT_PUBLIC_* into the client
# bundle during build, so they must be available NOW, not at runtime.
# docker-compose passes these in via `build.args`.
ARG NEXT_PUBLIC_API_URL
ARG NEXT_PUBLIC_OPERATOR_NAME
ARG NEXT_PUBLIC_OPERATOR_URL
ENV NEXT_PUBLIC_API_URL=$NEXT_PUBLIC_API_URL \
    NEXT_PUBLIC_OPERATOR_NAME=$NEXT_PUBLIC_OPERATOR_NAME \
    NEXT_PUBLIC_OPERATOR_URL=$NEXT_PUBLIC_OPERATOR_URL
RUN npm run build

# ---- runtime
FROM node:22-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1

# Standalone output bundles all required node_modules
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/public ./public

EXPOSE 3001
CMD ["node", "server.js"]
