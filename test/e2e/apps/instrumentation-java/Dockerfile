FROM eclipse-temurin:21.0.11_10-jdk@sha256:949f7645022e8cd5cd65eac4b342e7505c86b721b6338f8b677c6f2edc180262 AS build
WORKDIR /app
COPY SimpleHttpServer.java .
RUN javac SimpleHttpServer.java

FROM eclipse-temurin:21.0.11_10-jre@sha256:194b590649efee5555de080c634905a30c2885190210e37ada26370ed2a9c781
WORKDIR /app
COPY --from=build /app/*.class .
CMD ["java", "SimpleHttpServer"]
