package com.example.javaextension;

import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizer;
import io.opentelemetry.sdk.autoconfigure.spi.AutoConfigurationCustomizerProvider;

public final class JavaExtensionMarker implements AutoConfigurationCustomizerProvider {
    @Override
    public void customize(AutoConfigurationCustomizer autoConfiguration) {
        System.out.println("JAVA_EXTENSION_CUSTOMIZER_APPLIED");
    }
}
