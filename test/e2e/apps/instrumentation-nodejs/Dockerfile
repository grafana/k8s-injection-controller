FROM node:24.17.0-slim@sha256:c2d5ade763cacfb03fe9cb8e8af5d1be5041ff331921fa26a9b231ca3a4f780a
WORKDIR /app
COPY package.json .
RUN npm install
COPY app.js .
CMD ["node", "app.js"]
