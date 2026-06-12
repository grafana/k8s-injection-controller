FROM eclipse-temurin:21 AS build
WORKDIR /app
COPY SimpleHttpServer.java .
RUN javac SimpleHttpServer.java

FROM eclipse-temurin:21-jre
WORKDIR /app
COPY --from=build /app/*.class .
CMD ["java", "SimpleHttpServer"]
