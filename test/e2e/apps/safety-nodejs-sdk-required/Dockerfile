FROM node:24.22.1-slim@sha256:be8204d4bd327363357f532187b752ff62759c31bedf98e9e18fc08c9cbd605c
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js otel-init.js ./
CMD ["node", "--require", "/app/otel-init.js", "app.js"]
