FROM eclipse-temurin:25.0.5_8-jdk@sha256:884a2f8692cc1e2c7e21f95914adf0ae0eafee9a7eb663a12169cc44468256ec AS build
WORKDIR /app

# Compile the HTTP server app
COPY SimpleHttpServer.java .
RUN javac SimpleHttpServer.java

# Build a minimal no-op javaagent so JAVA_TOOL_OPTIONS=-javaagent: doesn't crash the JVM
RUN mkdir nop && \
    printf 'public class NopAgent { public static void premain(String a, java.lang.instrument.Instrumentation i) {} }' \
        > nop/NopAgent.java && \
    javac nop/NopAgent.java && \
    printf 'Premain-Class: NopAgent\n' > nop/MANIFEST.MF && \
    jar cfm nop-agent.jar nop/MANIFEST.MF -C nop NopAgent.class

FROM eclipse-temurin:25.0.5_8-jre@sha256:945247f9d092703e0c6be92243ef3c51640229e23ab34d353c8dcdc73e6cd4e6
WORKDIR /app
COPY --from=build /app/*.class .
COPY --from=build /app/nop-agent.jar /opt/nop-agent.jar
# This env var is what Beyla's scanner looks for to mark the JVM incompatible.
ENV JAVA_TOOL_OPTIONS="-javaagent:/opt/nop-agent.jar"
CMD ["java", "SimpleHttpServer"]
