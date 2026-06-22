FROM eclipse-temurin:21.0.11_10-jdk@sha256:762d16be48b976c97801d198e5085b1f6facb13718a1e8fb67cb25d0b25337e1 AS build
WORKDIR /app
COPY SimpleHttpServer.java .
RUN javac SimpleHttpServer.java

FROM eclipse-temurin:21.0.11_10-jre@sha256:8ec353b20d3aab0758572236b81b967c7077c40c4d0819ce97f9a1329d684603
WORKDIR /app
COPY --from=build /app/*.class .
CMD ["java", "SimpleHttpServer"]
