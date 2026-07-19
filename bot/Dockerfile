FROM node:20-bookworm-slim AS build

WORKDIR /app

COPY package.json ./
RUN npm install

COPY tsconfig.json ./
COPY src ./src

RUN npm run build


FROM node:20-bookworm-slim

WORKDIR /app
ENV NODE_ENV=production

COPY --from=build /app/node_modules ./node_modules
COPY --from=build /app/dist ./dist
COPY --from=build /app/package.json ./package.json

RUN mkdir -p /app/data

CMD ["node", "dist/index.js"]
