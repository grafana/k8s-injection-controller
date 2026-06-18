FROM eclipse-temurin:21.0.11_10-jdk AS build
WORKDIR /app
COPY SimpleHttpServer.java .
RUN javac SimpleHttpServer.java

FROM eclipse-temurin:21.0.11_10-jre
WORKDIR /app
COPY --from=build /app/*.class .
CMD ["java", "SimpleHttpServer"]
