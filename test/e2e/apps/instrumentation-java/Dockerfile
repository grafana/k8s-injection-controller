FROM eclipse-temurin:25.0.5_8-jdk@sha256:884a2f8692cc1e2c7e21f95914adf0ae0eafee9a7eb663a12169cc44468256ec AS build
WORKDIR /app
COPY SimpleHttpServer.java .
RUN javac SimpleHttpServer.java

FROM eclipse-temurin:25.0.5_8-jre@sha256:945247f9d092703e0c6be92243ef3c51640229e23ab34d353c8dcdc73e6cd4e6
WORKDIR /app
COPY --from=build /app/*.class .
CMD ["java", "SimpleHttpServer"]
