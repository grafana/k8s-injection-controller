FROM eclipse-temurin:21.0.11_10-jdk@sha256:762d16be48b976c97801d198e5085b1f6facb13718a1e8fb67cb25d0b25337e1 AS build
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

FROM eclipse-temurin:21.0.11_10-jre@sha256:8ec353b20d3aab0758572236b81b967c7077c40c4d0819ce97f9a1329d684603
WORKDIR /app
COPY --from=build /app/*.class .
COPY --from=build /app/nop-agent.jar /opt/nop-agent.jar
# This env var is what Beyla's scanner looks for to mark the JVM incompatible.
ENV JAVA_TOOL_OPTIONS="-javaagent:/opt/nop-agent.jar"
CMD ["java", "SimpleHttpServer"]
