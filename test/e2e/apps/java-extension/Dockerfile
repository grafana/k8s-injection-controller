FROM eclipse-temurin:25.0.5.1_2-jdk@sha256:ce2ef9d2d67a68de5e196d4de9d743327c44a10b57fe02ec39f572c652cacfd3 AS build
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

FROM eclipse-temurin:25.0.5.1_2-jre@sha256:4ed132f6832e9804b123e89a849a7b4502756ae64a4ee08ebab939d9d5d0a9fa
WORKDIR /app
RUN mkdir -p /__otel_java_extensions__
COPY --from=build /app/*.class .
COPY --from=build /app/java-extension.jar /extensions/java-extension.jar
CMD ["java", "SimpleHttpServer"]
