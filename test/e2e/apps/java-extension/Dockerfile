FROM eclipse-temurin:25.0.3_9-jdk@sha256:dfc0093e3dbf43dae57827111c6e374f5b44fac19a9451584b2b336b81474d64 AS build
WORKDIR /app

ARG OTEL_JAVA_AGENT_EXTENSION_API_VERSION=2.32.0-alpha
ARG OTEL_SDK_VERSION=1.66.0

RUN mkdir -p /tmp/deps /app/extensions/META-INF/services && \
    apt-get update && apt-get install -y --no-install-recommends ca-certificates wget && \
    rm -rf /var/lib/apt/lists/* && \
    wget --quiet --output-document=/tmp/deps/opentelemetry-javaagent-extension-api.jar \
      "https://repo.maven.apache.org/maven2/io/opentelemetry/javaagent/opentelemetry-javaagent-extension-api/${OTEL_JAVA_AGENT_EXTENSION_API_VERSION}/opentelemetry-javaagent-extension-api-${OTEL_JAVA_AGENT_EXTENSION_API_VERSION}.jar" \
      && wget --quiet --output-document=/tmp/deps/opentelemetry-sdk-extension-autoconfigure-spi.jar \
      "https://repo.maven.apache.org/maven2/io/opentelemetry/opentelemetry-sdk-extension-autoconfigure-spi/${OTEL_SDK_VERSION}/opentelemetry-sdk-extension-autoconfigure-spi-${OTEL_SDK_VERSION}.jar"

COPY SimpleHttpServer.java .
COPY extension-src/ extension-src/
RUN javac SimpleHttpServer.java && \
    javac -cp /tmp/deps/opentelemetry-javaagent-extension-api.jar:/tmp/deps/opentelemetry-sdk-extension-autoconfigure-spi.jar \
      -d /app/extensions extension-src/com/example/javaextension/JavaExtensionMarker.java && \
    cp extension-src/META-INF/services/io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider \
      /app/extensions/META-INF/services/ && \
    jar --create --file /app/java-extension.jar -C /app/extensions .

FROM eclipse-temurin:25.0.3_9-jre@sha256:7ea65de6187ad8fbcc0ad155950c38664a7371148bb3ccf1ec1e1b286b44ad08
WORKDIR /app
RUN mkdir -p /__otel_java_extensions__
COPY --from=build /app/*.class .
COPY --from=build /app/java-extension.jar /extensions/java-extension.jar
CMD ["java", "SimpleHttpServer"]
