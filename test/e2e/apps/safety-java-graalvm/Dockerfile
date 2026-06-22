FROM ghcr.io/graalvm/native-image-community:21.0.2@sha256:faed0fd6809b138254bdd6c7046e56894f4d9566ecbc7b0952aab43e65e16e0e AS build
WORKDIR /app
COPY NativeHttpServer.java .
RUN javac NativeHttpServer.java && \
    native-image --no-fallback -o nativehttpserver NativeHttpServer

FROM debian:12.14-slim@sha256:96e378d7e6531ac9a15ad505478fcc2e69f371b10f5cdf87857c4b8188404716
WORKDIR /app
COPY --from=build /app/nativehttpserver .
CMD ["./nativehttpserver"]
