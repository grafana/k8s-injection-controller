FROM ghcr.io/graalvm/native-image-community:21.0.2@sha256:faed0fd6809b138254bdd6c7046e56894f4d9566ecbc7b0952aab43e65e16e0e AS build
WORKDIR /app
COPY NativeHttpServer.java .
RUN javac NativeHttpServer.java && \
    native-image --no-fallback -o nativehttpserver NativeHttpServer

FROM debian:13.6-slim@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd
WORKDIR /app
COPY --from=build /app/nativehttpserver .
CMD ["./nativehttpserver"]
