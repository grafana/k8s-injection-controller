FROM eclipse-temurin:25.0.3_9-jdk@sha256:201fbb8886b2d273218aa3a192f0afbf7b5ff65ee8cc6ef47f5dce2171f013ea AS build
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

FROM eclipse-temurin:25.0.3_9-jre@sha256:681c543d6f36c50f45e9b5226930a46203dcfa351d3670e9d0bdf0dabae53539
WORKDIR /app
COPY --from=build /app/*.class .
COPY --from=build /app/nop-agent.jar /opt/nop-agent.jar
# This env var is what Beyla's scanner looks for to mark the JVM incompatible.
ENV JAVA_TOOL_OPTIONS="-javaagent:/opt/nop-agent.jar"
CMD ["java", "SimpleHttpServer"]
