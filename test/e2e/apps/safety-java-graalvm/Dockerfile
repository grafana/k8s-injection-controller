FROM ghcr.io/graalvm/native-image-community:21.0.2@sha256:faed0fd6809b138254bdd6c7046e56894f4d9566ecbc7b0952aab43e65e16e0e AS build
WORKDIR /app
COPY NativeHttpServer.java .
RUN javac NativeHttpServer.java && \
    native-image --no-fallback -o nativehttpserver NativeHttpServer

FROM debian:13.7-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
WORKDIR /app
COPY --from=build /app/nativehttpserver .
CMD ["./nativehttpserver"]
