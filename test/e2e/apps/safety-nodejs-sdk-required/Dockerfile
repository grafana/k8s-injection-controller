FROM node:24.20.0-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js otel-init.js ./
CMD ["node", "--require", "/app/otel-init.js", "app.js"]
