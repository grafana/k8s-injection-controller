# End-to-end tests

The Java extension ImageVolume test is opt-in because it requires a Kubernetes
1.35 Kind node. It uses Kind v0.31.0, which the suite installs only when no Kind
binary is available on `PATH`; upgrade any preinstalled Kind binary to v0.31.0
or newer. Run the focused test with:

```sh
mise run test-e2e-java-extension
```

The suite builds and loads `java-extension-app:dev` into Kind; the image is
used both as the test workload and as the extension image so the fixture stays
self-contained. This test verifies that admission adds the configured
`ImageVolumeSource` and passes its Java extension path to the injector through
`OTEL_INJECTOR_JAVAAGENT_EXTENSIONS`. The injector appends these paths to the
process's `OTEL_JAVAAGENT_EXTENSIONS`, preserving any value supplied directly
by the workload. It also verifies that an instrumented Java HTTP workload
starts and that the extension's `AutoConfigurationCustomizerProvider` runs
inside the upstream Java agent.

The runtime assertion uses SDK image `ghcr.io/grafana/beyla/inject-sdk-image:2026.11.12.0`,
which bundles OTel Injector v0.12.0 and supports
`OTEL_INJECTOR_JAVAAGENT_EXTENSIONS` ([released in v0.12.0](https://github.com/open-telemetry/opentelemetry-injector/releases/tag/v0.12.0)).
