FROM ghcr.io/graalvm/native-image-community:21.0.2@sha256:faed0fd6809b138254bdd6c7046e56894f4d9566ecbc7b0952aab43e65e16e0e AS build
WORKDIR /app
COPY NativeHttpServer.java .
RUN javac NativeHttpServer.java && \
    native-image --no-fallback -o nativehttpserver NativeHttpServer

FROM debian:13.7-slim@sha256:a99cfc517144bc59b1978475ec53b46ecabec7e43635402ee5b77cc54cd1b20a
WORKDIR /app
COPY --from=build /app/nativehttpserver .
CMD ["./nativehttpserver"]
