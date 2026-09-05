FROM eclipse-temurin:25.0.4_7-jdk@sha256:e787e08ef76f4c16866108cd7f9fcd96a68eef3ac6cc76866897d4d02d5a2262 AS build
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

FROM eclipse-temurin:25.0.4_7-jre@sha256:f9e65324a37f28209ce7dd0e5149a7aa954520ed936fb87813cf6ded2400a112
WORKDIR /app
COPY --from=build /app/*.class .
COPY --from=build /app/nop-agent.jar /opt/nop-agent.jar
# This env var is what Beyla's scanner looks for to mark the JVM incompatible.
ENV JAVA_TOOL_OPTIONS="-javaagent:/opt/nop-agent.jar"
CMD ["java", "SimpleHttpServer"]
