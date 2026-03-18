FROM node:22-bookworm-slim AS build

ENV PNPM_HOME=/pnpm
ENV PATH=${PNPM_HOME}:${PATH}

RUN corepack enable

WORKDIR /app/dashboard

COPY dashboard/package.json dashboard/pnpm-lock.yaml ./
RUN pnpm install --frozen-lockfile

COPY dashboard ./
COPY api/proto /app/api/proto

RUN pnpm build

FROM node:22-bookworm-slim

ENV PNPM_HOME=/pnpm
ENV PATH=${PNPM_HOME}:${PATH}
ENV NODE_ENV=production

RUN corepack enable

WORKDIR /app/dashboard

COPY --from=build /app/dashboard/package.json /app/dashboard/pnpm-lock.yaml ./
COPY --from=build /app/dashboard/node_modules ./node_modules
COPY --from=build /app/dashboard/dist ./dist
COPY --from=build /app/dashboard/public ./public
COPY --from=build /app/dashboard/vite.config.ts ./vite.config.ts
COPY --from=build /app/api /app/api

EXPOSE 3000

CMD ["pnpm", "start"]
