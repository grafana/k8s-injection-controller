FROM ghcr.io/graalvm/native-image-community:21.0.2 AS build
WORKDIR /app
COPY NativeHttpServer.java .
RUN javac NativeHttpServer.java && \
    native-image --no-fallback -o nativehttpserver NativeHttpServer

FROM debian:12.14-slim
WORKDIR /app
COPY --from=build /app/nativehttpserver .
CMD ["./nativehttpserver"]
