FROM eclipse-temurin:25.0.3_9-jdk@sha256:dfc0093e3dbf43dae57827111c6e374f5b44fac19a9451584b2b336b81474d64 AS build
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

FROM eclipse-temurin:25.0.3_9-jre@sha256:7ea65de6187ad8fbcc0ad155950c38664a7371148bb3ccf1ec1e1b286b44ad08
WORKDIR /app
COPY --from=build /app/*.class .
COPY --from=build /app/nop-agent.jar /opt/nop-agent.jar
# This env var is what Beyla's scanner looks for to mark the JVM incompatible.
ENV JAVA_TOOL_OPTIONS="-javaagent:/opt/nop-agent.jar"
CMD ["java", "SimpleHttpServer"]
